/*
Copyright 2020 The Machine Controller Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package nodecsrapprover

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"

	clusterv1alpha1 "k8c.io/machine-controller/sdk/apis/cluster/v1alpha1"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	certificatesv1client "k8s.io/client-go/kubernetes/typed/certificates/v1"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// ControllerName is name of the NodeCSRApprover controller.
	ControllerName = "node-csr-approver-controller"

	nodeUser       = "system:node"
	nodeUserPrefix = nodeUser + ":"

	nodeGroup          = "system:nodes"
	authenticatedGroup = "system:authenticated"
)

var (
	allowedUsages = []certificatesv1.KeyUsage{
		certificatesv1.UsageDigitalSignature,
		certificatesv1.UsageKeyEncipherment,
		certificatesv1.UsageServerAuth,
	}
)

type reconciler struct {
	ctrlruntimeclient.Client
	log *zap.SugaredLogger
	// Have to use the typed client because csr approval is a subresource
	// the dynamic client does not approve
	certClient certificatesv1client.CertificateSigningRequestInterface
}

func Add(mgr manager.Manager, log *zap.SugaredLogger) error {
	certClient, err := certificatesv1client.NewForConfig(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("failed to create certificate client: %w", err)
	}

	rec := &reconciler{
		Client:     mgr.GetClient(),
		log:        log.Named(ControllerName),
		certClient: certClient.CertificateSigningRequests(),
	}

	_, err = builder.ControllerManagedBy(mgr).
		Named(ControllerName).
		WithOptions(controller.Options{
			LogConstructor: func(*reconcile.Request) logr.Logger {
				// we log ourselves
				return zapr.NewLogger(zap.NewNop())
			},
		}).
		For(&certificatesv1.CertificateSigningRequest{}).
		Build(rec)

	return err
}

func (r *reconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	log := r.log.With("csr", request.NamespacedName)
	log.Debug("Reconciling")

	// Get the CSR object
	csr := &certificatesv1.CertificateSigningRequest{}
	if err := r.Get(ctx, request.NamespacedName, csr); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		log.Errorw("Failed to get CertificateSigningRequest", zap.Error(err))
		return reconcile.Result{}, err
	}

	err := r.reconcile(ctx, log, csr)
	if err != nil {
		log.Errorw("Reconciling failed", zap.Error(err))
	}

	return reconcile.Result{}, err
}

func (r *reconciler) reconcile(ctx context.Context, log *zap.SugaredLogger, csr *certificatesv1.CertificateSigningRequest) error {
	// If CSR is approved, skip it
	for _, condition := range csr.Status.Conditions {
		if condition.Type == certificatesv1.CertificateApproved {
			log.Debug("CSR already approved, skipping reconciling")
			return nil
		}
	}

	// Validate the CSR object and get the node name
	nodeName, err := r.validateCSRObject(csr)
	if err != nil {
		log.Debugw("Skipping reconciling CSR because object is invalid", zap.Error(err))
		return nil
	}

	// Get machine name for the appropriate node
	machine, found, err := r.getMachineForNode(ctx, nodeName)
	if err != nil {
		return fmt.Errorf("failed to get machine for node '%s': %w", nodeName, err)
	}
	if !found {
		return fmt.Errorf("no machine found for given node '%s'", nodeName)
	}

	// Parse the certificate request
	csrBlock, rest := pem.Decode(csr.Spec.Request)
	if csrBlock == nil {
		return fmt.Errorf("no certificate request found for the given CSR")
	}
	if len(rest) != 0 {
		return fmt.Errorf("found more than one PEM encoded block in the result")
	}
	certRequest, err := x509.ParseCertificateRequest(csrBlock.Bytes)
	if err != nil {
		return err
	}

	// Validate the certificate request
	if err := r.validateX509CSR(csr, certRequest, machine); err != nil {
		return fmt.Errorf("error validating the x509 certificate request: %w", err)
	}

	// Approve CSR
	nodeLog := log.With("node", nodeName)
	nodeLog.Debug("Approving CSR")
	approvalCondition := certificatesv1.CertificateSigningRequestCondition{
		Type:   certificatesv1.CertificateApproved,
		Reason: "machine-controller NodeCSRApprover controller approved node serving cert",
		Status: corev1.ConditionTrue,
	}
	csr.Status.Conditions = append(csr.Status.Conditions, approvalCondition)

	if _, err := r.certClient.UpdateApproval(ctx, csr.Name, csr, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("failed to approve CSR %q: %w", csr.Name, err)
	}

	nodeLog.Info("Successfully approved CSR")
	return nil
}

// validateCSRObject valides the CSR object and returns name of the node that requested the certificate.
func (r *reconciler) validateCSRObject(csr *certificatesv1.CertificateSigningRequest) (string, error) {
	// Get and validate the node name.
	if !strings.HasPrefix(csr.Spec.Username, nodeUserPrefix) {
		return "", fmt.Errorf("username must have the '%s' prefix", nodeUserPrefix)
	}
	nodeName := strings.TrimPrefix(csr.Spec.Username, nodeUserPrefix)
	if len(nodeName) == 0 {
		return "", fmt.Errorf("node name is empty")
	}

	// Ensure system:nodes and system:authenticated are in groups.
	if len(csr.Spec.Groups) < 2 {
		return "", fmt.Errorf("there are less than 2 groups")
	}
	if !sets.NewString(csr.Spec.Groups...).HasAll(nodeGroup, authenticatedGroup) {
		return "", fmt.Errorf("'%s' and/or '%s' are not in its groups", nodeGroup, authenticatedGroup)
	}

	// Check that present usages matching allowed usages
	for _, usage := range csr.Spec.Usages {
		if !isUsageInUsageList(usage, allowedUsages) {
			return "", fmt.Errorf("usage %v is not in the list of allowed usages (%v)", usage, allowedUsages)
		}
	}

	return nodeName, nil
}

// validateX509CSR validates the certificate request by comparing CN with username,
// and organization with groups.
func (r *reconciler) validateX509CSR(csr *certificatesv1.CertificateSigningRequest, certReq *x509.CertificateRequest, machine clusterv1alpha1.Machine) error {
	// Validate Subject CommonName.
	if certReq.Subject.CommonName != csr.Spec.Username {
		return fmt.Errorf("commonName '%s' is different then CSR username '%s'", certReq.Subject.CommonName, csr.Spec.Username)
	}

	// Validate Subject Organization.
	if len(certReq.Subject.Organization) != 1 {
		return fmt.Errorf("expected only one organization but got %d instead", len(certReq.Subject.Organization))
	}
	if certReq.Subject.Organization[0] != nodeGroup {
		return fmt.Errorf("organization '%s' doesn't match node group '%s'", certReq.Subject.Organization[0], nodeGroup)
	}

	machineAddressSet := sets.NewString(machine.Status.NodeRef.Name)
	for _, addr := range machine.Status.Addresses {
		machineAddressSet.Insert(addr.Address)
	}

	// Validate SAN DNS names.
	for _, dns := range certReq.DNSNames {
		if len(dns) == 0 {
			continue
		}
		if !machineAddressSet.Has(dns) {
			return fmt.Errorf("dns name '%s' cannot be associated with node '%s'", dns, machine.Status.NodeRef.Name)
		}
	}

	// Validate SAN IP addresses
	for _, ip := range certReq.IPAddresses {
		if len(ip) == 0 {
			continue
		}
		if !machineAddressSet.Has(ip.String()) {
			return fmt.Errorf("ip address '%v' cannot be associated with node '%s'", ip, machine.Status.NodeRef.Name)
		}
	}

	return nil
}

func (r *reconciler) getMachineForNode(ctx context.Context, nodeName string) (clusterv1alpha1.Machine, bool, error) {
	// List all Machines in all namespaces.
	machines := &clusterv1alpha1.MachineList{}
	if err := r.List(ctx, machines); err != nil {
		return clusterv1alpha1.Machine{}, false, fmt.Errorf("failed to list all machine objects: %w", err)
	}

	for _, machine := range machines.Items {
		if machine.Status.NodeRef != nil && machine.Status.NodeRef.Name == nodeName {
			return machine, true, nil
		}
	}

	return clusterv1alpha1.Machine{}, false, fmt.Errorf("failed to get machine for given node name '%s'", nodeName)
}

func isUsageInUsageList(usage certificatesv1.KeyUsage, usageList []certificatesv1.KeyUsage) bool {
	for _, usageListItem := range usageList {
		if usage == usageListItem {
			return true
		}
	}
	return false
}
