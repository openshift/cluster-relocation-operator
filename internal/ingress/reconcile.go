package ingress

import (
	"context"
	"fmt"
	"strings"

	rhsysenggithubiov1beta1 "github.com/RHsyseng/cluster-relocation-operator/api/v1beta1"
	secrets "github.com/RHsyseng/cluster-relocation-operator/internal/secrets"
	"github.com/RHsyseng/cluster-relocation-operator/internal/util"
	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

//+kubebuilder:rbac:groups="",resources=secrets,verbs=create;update;get;list;watch
//+kubebuilder:rbac:groups=operator.openshift.io,resources=ingresscontrollers,verbs=patch;get;list;watch
//+kubebuilder:rbac:groups=config.openshift.io,resources=ingresses,verbs=patch;get;list;watch
//+kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=list;delete;watch

func Reconcile(ctx context.Context, c client.Client, scheme *runtime.Scheme, relocation *rhsysenggithubiov1beta1.ClusterRelocation, logger logr.Logger) error {
	// Configure certificates with the new domain name for the ingress
	var origSecretName string
	var origSecretNamespace string
	if relocation.Spec.IngressCertRef == nil {
		// If they haven't specified an IngressCertRef, we generate a self-signed certificate for them
		origSecretName = "generated-ingress-secret"
		origSecretNamespace = rhsysenggithubiov1beta1.IngressNamespace
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: origSecretName, Namespace: origSecretNamespace}}

		op, err := controllerutil.CreateOrUpdate(ctx, c, secret, func() error {
			_, ok := secret.Data[corev1.TLSCertKey]
			// Check if the secret already has a key set
			// If there is no key set, generate one
			// This is done so that we don't generate a new certificate each time Reconcile runs
			if !ok {
				logger.Info("generating new TLS cert for Ingresses")
				var err error
				secret.Data, err = secrets.GenerateTLSKeyPair(relocation.Spec.Domain, "*.apps")
				if err != nil {
					return err
				}
			} else {
				logger.Info("TLS cert already exists for Ingresses")
				commonName, err := secrets.GetCertCommonName(secret.Data[corev1.TLSCertKey])
				if err != nil {
					return err
				}
				if commonName != fmt.Sprintf("*.apps.%s", relocation.Spec.Domain) {
					logger.Info("Domain name has changed, generating new TLS certificate for Ingresses")
					var err error
					secret.Data, err = secrets.GenerateTLSKeyPair(relocation.Spec.Domain, "*.apps")
					if err != nil {
						return err
					}
				}
			}
			secret.Type = corev1.SecretTypeTLS
			// Set the controller as the owner so that the secret is deleted along with the CR
			return controllerutil.SetControllerReference(relocation, secret, scheme)
		})
		if err != nil {
			return err
		}
		if op != controllerutil.OperationResultNone {
			logger.Info("Self-signed Ingress TLS cert modified", "OperationResult", op)
		}

		secretName := origSecretName
		copySettings := secrets.SecretCopySettings{
			OwnOriginal:                  false,
			OriginalOwnedByController:    false,
			OwnDestination:               true,
			DestinationOwnedByController: true,
		}
		op, err = secrets.CopySecret(ctx, c, relocation, scheme, origSecretName, origSecretNamespace, secretName, rhsysenggithubiov1beta1.ConfigNamespace, copySettings)
		if err != nil {
			return err
		}
		if op != controllerutil.OperationResultNone {
			logger.Info(fmt.Sprintf("Generated Ingress cert copied to %s", rhsysenggithubiov1beta1.ConfigNamespace), "OperationResult", op)
		}
	} else {
		if relocation.Spec.IngressCertRef.Name == "" || relocation.Spec.IngressCertRef.Namespace == "" {
			return fmt.Errorf("must specify secret name and namespace")
		}
		if err := secrets.ValidateSecretType(ctx, c, relocation.Spec.IngressCertRef, corev1.SecretTypeTLS); err != nil {
			return err
		}

		origSecretName = relocation.Spec.IngressCertRef.Name
		origSecretNamespace = relocation.Spec.IngressCertRef.Namespace
		logger.Info("Using user provided Ingress certificate", "namespace", origSecretNamespace, "name", origSecretName)

		// The certificate must be in the openshift-ingress namespace
		// as well as the openshift-config namespace, so we copy it
		secretName := "copied-ingress-secret"
		// Copy the secret into the openshift-ingress namespace
		// the original may be owned by another controller (cert-manager for example)
		// we add non-controller ownership to this secret, in order to watch it.
		// our controller should own the destination secret
		copySettings := secrets.SecretCopySettings{
			OwnOriginal:                  true,
			OriginalOwnedByController:    false,
			OwnDestination:               true,
			DestinationOwnedByController: true,
		}
		op, err := secrets.CopySecret(ctx, c, relocation, scheme, origSecretName, origSecretNamespace, secretName, rhsysenggithubiov1beta1.IngressNamespace, copySettings)
		if err != nil {
			return err
		}
		if op != controllerutil.OperationResultNone {
			logger.Info(fmt.Sprintf("User provided Ingress cert copied to %s", rhsysenggithubiov1beta1.IngressNamespace), "OperationResult", op)
		}

		// Copy the secret into the openshift-config namespace
		op, err = secrets.CopySecret(ctx, c, relocation, scheme, origSecretName, origSecretNamespace, secretName, rhsysenggithubiov1beta1.ConfigNamespace, copySettings)
		if err != nil {
			return err
		}
		if op != controllerutil.OperationResultNone {
			logger.Info(fmt.Sprintf("User provided Ingress cert copied to %s", rhsysenggithubiov1beta1.ConfigNamespace), "OperationResult", op)
		}
		origSecretName = secretName
	}

	// Define the IngressController's namespace and name
	namespace := "openshift-ingress-operator"
	name := "default"

	ingressController := &operatorv1.IngressController{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	op, err := controllerutil.CreateOrPatch(ctx, c, ingressController, func() error {
		ingressController.Spec.DefaultCertificate = &corev1.LocalObjectReference{Name: origSecretName}
		return nil
	})
	if err != nil {
		return err
	}

	if op != controllerutil.OperationResultNone {
		logger.Info("IngressController modified", "OperationResult", op)
	}

	ingress := &configv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}}
	op, err = controllerutil.CreateOrPatch(ctx, c, ingress, func() error {
		ingress.Spec.AppsDomain = fmt.Sprintf("apps.%s", relocation.Spec.Domain)
		ingress.Spec.ComponentRoutes = []configv1.ComponentRouteSpec{
			{
				Hostname:  configv1.Hostname(fmt.Sprintf("console-openshift-console.apps.%s", relocation.Spec.Domain)),
				Name:      "console",
				Namespace: "openshift-console",
				ServingCertKeyPairSecret: configv1.SecretNameReference{
					Name: origSecretName,
				},
			},
			{
				Hostname:  configv1.Hostname(fmt.Sprintf("downloads-openshift-console.apps.%s", relocation.Spec.Domain)),
				Name:      "downloads",
				Namespace: "openshift-console",
				ServingCertKeyPairSecret: configv1.SecretNameReference{
					Name: origSecretName,
				},
			},
			{
				Hostname:  configv1.Hostname(fmt.Sprintf("oauth-openshift.apps.%s", relocation.Spec.Domain)),
				Name:      "oauth-openshift",
				Namespace: "openshift-authentication",
				ServingCertKeyPairSecret: configv1.SecretNameReference{
					Name: origSecretName,
				},
			},
		}
		return err
	})
	if err != nil {
		return err
	}

	if op != controllerutil.OperationResultNone {
		logger.Info("Ingress domain aliases modified", "OperationResult", op)
	}

	return nil
}

// We modified the Ingress Controller and Ingress Cluster resources, but we don't own it
// Therefore, we need to use a finalizer to put it back the way we found it if the CR is deleted
func Cleanup(ctx context.Context, c client.Client, logger logr.Logger) error {
	namespace := "openshift-ingress-operator"
	name := "default"
	ingressController := &operatorv1.IngressController{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	op, err := controllerutil.CreateOrPatch(ctx, c, ingressController, func() error {
		ingressController.Spec.DefaultCertificate = nil
		return nil
	})
	if err != nil {
		return err
	}
	if op != controllerutil.OperationResultNone {
		logger.Info("Ingress Controller reverted to original state", "OperationResult", op)
	}
	ingress := &configv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}}
	op, err = controllerutil.CreateOrPatch(ctx, c, ingress, func() error {
		ingress.Spec.AppsDomain = ""
		ingress.Spec.ComponentRoutes = nil
		return nil
	})
	if err != nil {
		return err
	}
	if op != controllerutil.OperationResultNone {
		logger.Info("Cluster Ingress reverted to original state", "OperationResult", op)
	}

	return nil
}

func ResetRoutes(ctx context.Context, c client.Client, domainName string, logger logr.Logger) error {
	routes := &routev1.RouteList{}
	if err := c.List(ctx, routes); err != nil {
		return err
	}

	if err := util.WaitForCO(ctx, c, logger, "openshift-apiserver"); err != nil {
		return err
	}

	for _, v := range routes.Items {
		if v.Namespace == "openshift-console" || v.Namespace == "openshift-authentication" || v.Namespace == "open-cluster-management-agent-addon" {
			// open-cluster-management-agent-addon is ignored because right now the Klusterlet Add-on ignores the "appsDomain" setting
			// A PR has been opened to correct this: https://github.com/stolostron/multicloud-operators-foundation/pull/642
			// without this fix, the Route created by the Klusterlet is always re-created with the original domain
			continue
		}
		for _, w := range v.Status.Ingress {
			if w.RouterName == "default" { // check Routes associated with the default Ingress Controller
				if !strings.Contains(w.Host, domainName) { // hostname for this route needs to be updated
					if err := c.Delete(ctx, &v); err != nil {
						return err
					}
					logger.Info("Deleted Route so that it can be re-created with new domain", "Route", v.Name, "Host", w.Host, "namespace", v.Namespace)
				}
			}
		}
	}
	return nil
}
