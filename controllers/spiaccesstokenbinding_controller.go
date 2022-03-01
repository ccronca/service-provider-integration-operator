/*
Copyright 2021.

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

package controllers

import (
	"context"
	"fmt"

	"github.com/redhat-appstudio/service-provider-integration-operator/pkg/spi-shared/tokenstorage"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/redhat-appstudio/service-provider-integration-operator/pkg/sync"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	api "github.com/redhat-appstudio/service-provider-integration-operator/api/v1beta1"
	"github.com/redhat-appstudio/service-provider-integration-operator/pkg/config"
	"github.com/redhat-appstudio/service-provider-integration-operator/pkg/serviceprovider"
)

var spiAccessTokenBindingLog = log.Log.WithName("spiaccesstokenbinding-controller")

var (
	secretDiffOpts = cmp.Options{
		cmpopts.IgnoreFields(corev1.Secret{}, "TypeMeta", "ObjectMeta"),
	}
)

// SPIAccessTokenBindingReconciler reconciles a SPIAccessTokenBinding object
type SPIAccessTokenBindingReconciler struct {
	client.Client
	Scheme                 *runtime.Scheme
	TokenStorage           tokenstorage.TokenStorage
	syncer                 sync.Syncer
	ServiceProviderFactory serviceprovider.Factory
}

//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=spiaccesstokenbindings,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=spiaccesstokenbindings/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=appstudio.redhat.com,resources=spiaccesstokenbindings/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;watch;create;update;list;delete

// SetupWithManager sets up the controller with the Manager.
func (r *SPIAccessTokenBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.syncer = sync.New(mgr.GetClient())
	return ctrl.NewControllerManagedBy(mgr).
		For(&api.SPIAccessTokenBinding{}).
		Owns(&corev1.Secret{}).
		Watches(&source.Kind{Type: &api.SPIAccessToken{}}, handler.EnqueueRequestsFromMapFunc(func(o client.Object) []reconcile.Request {
			bindings := &api.SPIAccessTokenBindingList{}
			if err := r.Client.List(context.TODO(), bindings, client.InNamespace(o.GetNamespace())); err != nil {
				spiAccessTokenBindingLog.Error(err, "failed to list SPIAccessTokenBindings while determining the ones linked to SPIAccessToken",
					"SPIAccessTokenName", o.GetName(), "SPIAccessTokenNamespace", o.GetNamespace())
				return []reconcile.Request{}
			}
			ret := make([]reconcile.Request, len(bindings.Items))
			for _, b := range bindings.Items {
				ret = append(ret, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      b.Name,
						Namespace: b.Namespace,
					},
				})
			}
			return ret
		})).
		Complete(r)
}

func (r *SPIAccessTokenBindingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lg := log.FromContext(ctx)

	lg.Info("Reconciling")

	binding := api.SPIAccessTokenBinding{}

	if err := r.Get(ctx, req.NamespacedName, &binding); err != nil {
		if errors.IsNotFound(err) {
			lg.Info("object not found")
			return ctrl.Result{}, nil
		}

		lg.Error(err, "failed to get the object")
		return ctrl.Result{}, NewReconcileError(err, "failed to read the object")
	}

	if binding.DeletionTimestamp != nil {
		lg.Info("object is being deleted")
		return ctrl.Result{}, nil
	}

	if binding.Status.Phase == "" {
		binding.Status.Phase = api.SPIAccessTokenBindingPhaseAwaitingTokenData
	}

	sp, rerr := r.getServiceProvider(ctx, &binding)
	if rerr != nil {
		lg.Error(rerr, "unable to get the service provider")
		return ctrl.Result{}, rerr
	}

	var token *api.SPIAccessToken

	if binding.Status.LinkedAccessTokenName == "" {
		var err error
		token, err = r.linkToken(ctx, sp, &binding)
		if err != nil {
			lg.Error(err, "unable to link the token")
			return ctrl.Result{}, NewReconcileError(err, "failed to link the token")
		}
	} else {
		token = &api.SPIAccessToken{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: binding.Status.LinkedAccessTokenName, Namespace: binding.Namespace}, token); err != nil {
			if errors.IsNotFound(err) {
				binding.Status.LinkedAccessTokenName = ""
				r.updateStatusError(ctx, &binding, api.SPIAccessTokenBindingErrorReasonLinkedToken, err)
			}
			lg.Error(err, "failed to fetch the linked token")
			return ctrl.Result{}, err
		}

		if token.Status.Phase == api.SPIAccessTokenPhaseReady && binding.Status.SyncedObjectRef.Name == "" {
			// we've not yet synced the token... let's check that it fulfills the reqs
			newToken, err := sp.LookupToken(ctx, r.Client, &binding)
			if err != nil {
				return ctrl.Result{}, NewReconcileError(err, "failed to lookup token before definitely assiging it to the binding")
			}
			if newToken == nil {
				// the token that we are linked to is ready but doesn't match the criteria of the binding.
				// We can't do much here - the user granted the token the access we requested, but we still don't match
				binding.Status.Phase = api.SPIAccessTokenBindingPhaseError
				binding.Status.OAuthUrl = ""
				r.updateStatusError(ctx, &binding, api.SPIAccessTokenBindingErrorReasonLinkedToken, fmt.Errorf("linked token doesn't match the criteria"))
				return ctrl.Result{}, nil
			}

			if newToken.UID != token.UID {
				if err = r.persistWithMatchingLabels(ctx, &binding, newToken); err != nil {
					return ctrl.Result{}, NewReconcileError(err, "failed to persist the newly matching token")
				}
				token = newToken
			}
		} else if token.Status.Phase != api.SPIAccessTokenPhaseReady {
			// let's try to do a lookup in case another token started matching our reqs
			// this time, only do the lookup in SP and don't create a new token if no match found
			//
			// yes, this can create garbage - abandoned tokens, see https://issues.redhat.com/browse/SVPI-65
			newToken, err := sp.LookupToken(ctx, r.Client, &binding)
			if err != nil {
				lg.Error(err, "failed lookup when trying to reassign linked token")
				// we're not returning the error or writing the status here, because the binding already has a valid
				// linked token.
			} else if newToken != nil {
				// yay, we found another match! Let's persist that change otherwise we could enter a weird state below,
				// where we would be syncing a secret that comes from a token that is not linked
				if err = r.persistWithMatchingLabels(ctx, &binding, newToken); err != nil {
					return ctrl.Result{}, NewReconcileError(err, "failed to persist the newly matching token")
				}
				token = newToken
			}
		}
	}

	binding.Status.OAuthUrl = token.Status.OAuthUrl

	switch token.Status.Phase {
	case api.SPIAccessTokenPhaseReady:
		ref, err := r.syncSecret(ctx, sp, &binding, token)
		if err != nil {
			lg.Error(err, "unable to sync the secret")
			return ctrl.Result{}, NewReconcileError(err, "failed to sync the secret")
		}
		binding.Status.SyncedObjectRef = ref
		binding.Status.Phase = api.SPIAccessTokenBindingPhaseInjected
	case api.SPIAccessTokenPhaseAwaitingTokenData:
		binding.Status.Phase = api.SPIAccessTokenBindingPhaseAwaitingTokenData
	}

	if err := r.updateStatusSuccess(ctx, &binding); err != nil {
		lg.Error(err, "unable to update the status")
		return ctrl.Result{}, NewReconcileError(err, "failed to update the status")
	}

	lg.Info("reconciliation complete")

	return ctrl.Result{}, nil
}

// getServiceProvider obtains the service provider instance according to the repository URL from the binding's spec.
// The status of the binding is immediately persisted with an error if the service provider cannot be determined.
func (r *SPIAccessTokenBindingReconciler) getServiceProvider(ctx context.Context, binding *api.SPIAccessTokenBinding) (serviceprovider.ServiceProvider, *ReconcileError) {
	serviceProvider, err := r.ServiceProviderFactory.FromRepoUrl(binding.Spec.RepoUrl)
	if err != nil {
		r.updateStatusError(ctx, binding, api.SPIAccessTokenBindingErrorReasonUnknownServiceProviderType, err)
		return nil, NewReconcileError(err, "failed to find the service provider")
	}

	return serviceProvider, nil
}

// linkToken updates the binding with a link to an SPIAccessToken object that should hold the token data. If no
// suitable SPIAccessToken object exists, it is created (in an awaiting state) and linked.
func (r *SPIAccessTokenBindingReconciler) linkToken(ctx context.Context, sp serviceprovider.ServiceProvider, binding *api.SPIAccessTokenBinding) (*api.SPIAccessToken, error) {
	token, err := sp.LookupToken(ctx, r.Client, binding)
	if err != nil {
		r.updateStatusError(ctx, binding, api.SPIAccessTokenBindingErrorReasonTokenLookup, err)
		return nil, NewReconcileError(err, "failed to lookup the token in the service provider")
	}

	if token == nil {
		log.FromContext(ctx).Info("creating a new token because none found for binding")

		serviceProviderUrl := sp.GetBaseUrl()
		if err != nil {
			r.updateStatusError(ctx, binding, api.SPIAccessTokenBindingErrorReasonUnknownServiceProviderType, err)
			return nil, NewReconcileError(err, "failed to determine the service provider URL from the repo")
		}

		// create the token (and let its webhook and controller finish the setup)
		token = &api.SPIAccessToken{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "generated-spi-access-token-",
				Namespace:    binding.Namespace,
			},
			Spec: api.SPIAccessTokenSpec{
				ServiceProviderType: sp.GetType(),
				Permissions:         binding.Spec.Permissions,
				ServiceProviderUrl:  serviceProviderUrl,
			},
		}

		if err := r.Client.Create(ctx, token); err != nil {
			r.updateStatusError(ctx, binding, api.SPIAccessTokenBindingErrorReasonLinkedToken, err)
			return nil, NewReconcileError(err, "failed to create the token")
		}
	}

	// we need to have this label so that updates to the linked SPIAccessToken are reflected here, too... We're setting
	// up the watch to use the label to limit the scope...
	if err := r.persistWithMatchingLabels(ctx, binding, token); err != nil {
		return nil, err
	}

	return token, nil
}

func (r *SPIAccessTokenBindingReconciler) persistWithMatchingLabels(ctx context.Context, binding *api.SPIAccessTokenBinding, token *api.SPIAccessToken) error {
	if binding.Labels[config.SPIAccessTokenLinkLabel] != token.Name {
		if binding.Labels == nil {
			binding.Labels = map[string]string{}
		}
		binding.Labels[config.SPIAccessTokenLinkLabel] = token.Name

		if err := r.Client.Update(ctx, binding); err != nil {
			r.updateStatusError(ctx, binding, api.SPIAccessTokenBindingErrorReasonLinkedToken, err)
			return NewReconcileError(err, "failed to update the binding with the token link")
		}
	}

	if binding.Status.LinkedAccessTokenName != token.Name {
		binding.Status.LinkedAccessTokenName = token.Name
		binding.Status.OAuthUrl = token.Status.OAuthUrl
		if err := r.updateStatusSuccess(ctx, binding); err != nil {
			r.updateStatusError(ctx, binding, api.SPIAccessTokenBindingErrorReasonLinkedToken, err)
			return NewReconcileError(err, "failed to update the binding status with the token link")
		}
	}

	return nil
}

// updateStatusError updates the status of the binding with the provided error
func (r *SPIAccessTokenBindingReconciler) updateStatusError(ctx context.Context, binding *api.SPIAccessTokenBinding, reason api.SPIAccessTokenBindingErrorReason, err error) {
	binding.Status.ErrorMessage = err.Error()
	binding.Status.ErrorReason = reason
	if err := r.Client.Status().Update(ctx, binding); err != nil {
		log.FromContext(ctx).Error(err, "failed to update the status with error", "reason", reason, "error", err)
	}
}

// updateStatusSuccess updates the status of the binding as successful, clearing any previous error state.
func (r *SPIAccessTokenBindingReconciler) updateStatusSuccess(ctx context.Context, binding *api.SPIAccessTokenBinding) error {
	binding.Status.ErrorMessage = ""
	binding.Status.ErrorReason = ""
	if err := r.Client.Status().Update(ctx, binding); err != nil {
		return NewReconcileError(err, "failed to update status")
	}
	return nil
}

// syncSecret creates/updates/deletes the secret specified in the binding with the token data and returns a reference
// to the secret.
func (r *SPIAccessTokenBindingReconciler) syncSecret(ctx context.Context, sp serviceprovider.ServiceProvider, binding *api.SPIAccessTokenBinding, tokenObject *api.SPIAccessToken) (api.TargetObjectRef, error) {
	token, err := r.TokenStorage.Get(ctx, tokenObject)
	if err != nil {
		r.updateStatusError(ctx, binding, api.SPIAccessTokenBindingErrorReasonTokenRetrieval, err)
		return api.TargetObjectRef{}, NewReconcileError(err, "failed to get the token data from token storage")
	}

	if token == nil {
		r.updateStatusError(ctx, binding, api.SPIAccessTokenBindingErrorReasonTokenRetrieval, err)
		return api.TargetObjectRef{}, fmt.Errorf("access token data not found")
	}

	var userId, userName string
	var scopes []string

	if tokenObject.Status.TokenMetadata != nil {
		userName = tokenObject.Status.TokenMetadata.Username
		userId = tokenObject.Status.TokenMetadata.UserId
		scopes = tokenObject.Status.TokenMetadata.Scopes
	}

	at := AccessTokenMapper{
		Name:                    tokenObject.Name,
		Token:                   token.AccessToken,
		ServiceProviderUrl:      tokenObject.Spec.ServiceProviderUrl,
		ServiceProviderUserName: userName,
		ServiceProviderUserId:   userId,
		UserId:                  "",
		ExpiredAfter:            &token.Expiry,
		Scopes:                  scopes,
	}

	stringData := at.toSecretType(binding.Spec.Secret.Type)
	at.fillByMapping(&binding.Spec.Secret.Fields, stringData)

	// copy the string data into the byte-array data so that sync works reliably. If we didn't sync, we could have just
	// used the Secret.StringData, but Sync gives us other goodies.
	// So let's bite the bullet and convert manually here.
	data := make(map[string][]byte, len(stringData))
	for k, v := range stringData {
		data[k] = []byte(v)
	}

	secretName := binding.Status.SyncedObjectRef.Name
	if secretName == "" {
		secretName = binding.Spec.Secret.Name
	}

	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Secret",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        secretName,
			Namespace:   binding.GetNamespace(),
			Labels:      binding.Spec.Secret.Labels,
			Annotations: binding.Spec.Secret.Annotations,
		},
		Data: data,
		Type: binding.Spec.Secret.Type,
	}

	if secret.Name == "" {
		secret.GenerateName = binding.Name + "-secret-"
	}

	_, obj, err := r.syncer.Sync(ctx, binding, secret, secretDiffOpts)
	if err != nil {
		r.updateStatusError(ctx, binding, api.SPIAccessTokenBindingErrorReasonTokenSync, err)
		return api.TargetObjectRef{}, NewReconcileError(err, "failed to sync the secret with the token data")
	}
	return toObjectRef(obj), nil
}

// toObjectRef creates a reference to a kubernetes object within the same namespace (i.e, a struct containing the name,
// kind and API version of the target object).
func toObjectRef(obj client.Object) api.TargetObjectRef {
	apiVersion, kind := obj.GetObjectKind().GroupVersionKind().ToAPIVersionAndKind()
	return api.TargetObjectRef{
		Name:       obj.GetName(),
		Kind:       kind,
		ApiVersion: apiVersion,
	}
}