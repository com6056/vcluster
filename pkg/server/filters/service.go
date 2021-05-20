package filters

import (
	"context"
	"fmt"
	"github.com/loft-sh/vcluster/pkg/util/clienthelper"
	"github.com/loft-sh/vcluster/pkg/util/encoding"
	"github.com/loft-sh/vcluster/pkg/util/random"
	requestpkg "github.com/loft-sh/vcluster/pkg/util/request"
	"github.com/loft-sh/vcluster/pkg/util/translate"
	"github.com/pkg/errors"
	"io/ioutil"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversionscheme "k8s.io/apimachinery/pkg/apis/meta/internalversion/scheme"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/endpoints/handlers/negotiation"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/klog"
	"net/http"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func WithServiceCreateRedirect(handler http.Handler, localManager ctrl.Manager, virtualManager ctrl.Manager, targetNamespace string) http.Handler {
	decoder := encoding.NewDecoder(localManager.GetScheme(), false)
	s := serializer.NewCodecFactory(virtualManager.GetScheme())
	uncachedLocalClient, err := client.New(localManager.GetConfig(), client.Options{
		Scheme: localManager.GetScheme(),
		Mapper: localManager.GetRESTMapper(),
	})
	if err != nil {
		panic(err)
	}
	uncachedVirtualClient, err := client.New(virtualManager.GetConfig(), client.Options{
		Scheme: virtualManager.GetScheme(),
		Mapper: virtualManager.GetRESTMapper(),
	})
	if err != nil {
		panic(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		info, ok := request.RequestInfoFrom(req.Context())
		if !ok {
			requestpkg.FailWithStatus(w, req, http.StatusInternalServerError, fmt.Errorf("request info is missing"))
			return
		}

		userInfo, ok := request.UserFrom(req.Context())
		if !ok {
			requestpkg.FailWithStatus(w, req, http.StatusInternalServerError, fmt.Errorf("user info is missing"))
			return
		}

		if info.APIVersion == corev1.SchemeGroupVersion.Version && info.APIGroup == corev1.SchemeGroupVersion.Group && info.Resource == "services" && info.Subresource == "" {
			if info.Verb == "create" {
				options := &metav1.CreateOptions{}
				if err := metainternalversionscheme.ParameterCodec.DecodeParameters(req.URL.Query(), corev1.SchemeGroupVersion, options); err != nil {
					responsewriters.ErrorNegotiated(err, s, corev1.SchemeGroupVersion, w, req)
					return
				}

				if len(options.DryRun) == 0 {
					uncachedVirtualImpersonatingClient, err := clienthelper.NewImpersonatingClient(virtualManager.GetConfig(), virtualManager.GetRESTMapper(), userInfo, virtualManager.GetScheme())
					if err != nil {
						responsewriters.ErrorNegotiated(err, s, corev1.SchemeGroupVersion, w, req)
						return
					}

					svc, err := createService(req, decoder, uncachedLocalClient, uncachedVirtualImpersonatingClient, info.Namespace, targetNamespace)
					if err != nil {
						responsewriters.ErrorNegotiated(err, s, corev1.SchemeGroupVersion, w, req)
						return
					}

					responsewriters.WriteObjectNegotiated(s, negotiation.DefaultEndpointRestrictions, corev1.SchemeGroupVersion, w, req, http.StatusCreated, svc)
					return
				}
			} else if info.Verb == "update" {
				options := &metav1.UpdateOptions{}
				if err := metainternalversionscheme.ParameterCodec.DecodeParameters(req.URL.Query(), corev1.SchemeGroupVersion, options); err != nil {
					responsewriters.ErrorNegotiated(err, s, corev1.SchemeGroupVersion, w, req)
					return
				}

				if len(options.DryRun) == 0 {
					// the only case we have to intercept this is when the service type changes from ExternalName
					vService := &corev1.Service{}
					err := uncachedVirtualClient.Get(req.Context(), client.ObjectKey{Namespace: info.Namespace, Name: info.Name}, vService)
					if err != nil {
						responsewriters.ErrorNegotiated(err, s, corev1.SchemeGroupVersion, w, req)
						return
					}

					if vService.Spec.Type == corev1.ServiceTypeExternalName {
						uncachedVirtualImpersonatingClient, err := clienthelper.NewImpersonatingClient(virtualManager.GetConfig(), virtualManager.GetRESTMapper(), userInfo, virtualManager.GetScheme())
						if err != nil {
							responsewriters.ErrorNegotiated(err, s, corev1.SchemeGroupVersion, w, req)
							return
						}

						svc, err := updateService(req, decoder, uncachedLocalClient, uncachedVirtualImpersonatingClient, vService, targetNamespace)
						if err != nil {
							responsewriters.ErrorNegotiated(err, s, corev1.SchemeGroupVersion, w, req)
							return
						}

						responsewriters.WriteObjectNegotiated(s, negotiation.DefaultEndpointRestrictions, corev1.SchemeGroupVersion, w, req, http.StatusOK, svc)
						return
					}
				}
			}
		}

		handler.ServeHTTP(w, req)
	})
}

func updateService(req *http.Request, decoder encoding.Decoder, localClient client.Client, virtualClient client.Client, oldVService *corev1.Service, targetNamespace string) (runtime.Object, error) {
	// authorization will be done at this point already, so we can redirect the request to the physical cluster
	rawObj, err := ioutil.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}

	svc, err := decoder.Decode(rawObj)
	if err != nil {
		return nil, err
	}

	newVService, ok := svc.(*corev1.Service)
	if !ok {
		return nil, fmt.Errorf("expected service object")
	}

	// type has not changed, we just update here
	if newVService.ResourceVersion != oldVService.ResourceVersion || newVService.Spec.Type == oldVService.Spec.Type || newVService.Spec.ClusterIP != "" {
		err = virtualClient.Update(req.Context(), newVService)
		if err != nil {
			return nil, err
		}

		return newVService, nil
	}

	// we use a background context from now on as this is a critical operation
	ctx := context.Background()

	// okay now we have to change the physical service
	pService := &corev1.Service{}
	err = localClient.Get(ctx, client.ObjectKey{Namespace: targetNamespace, Name: translate.PhysicalName(oldVService.Name, oldVService.Namespace)}, pService)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil, kerrors.NewNotFound(corev1.Resource("services"), oldVService.Name)
		}

		return nil, err
	}

	// we try to patch the service as this has the best chances to go through
	orignialPService := pService.DeepCopy()
	pService.Spec.Ports = newVService.Spec.Ports
	pService.Spec.Type = newVService.Spec.Type
	pService.Spec.ClusterIP = ""
	err = localClient.Patch(ctx, pService, client.MergeFrom(orignialPService))
	if err != nil {
		return nil, err
	}

	// now we have the cluster ip that we can apply to the new service
	newVService.Spec.ClusterIP = pService.Spec.ClusterIP
	err = virtualClient.Update(ctx, newVService)
	if err != nil {
		// this is actually worst case that can happen, as we have somehow now a really strange
		// state in the cluster. This needs to be cleaned up by the controller via delete and create
		// and we delete the physical service here. Maybe there is a better solution to this, but for
		// now it works
		_ = localClient.Delete(ctx, pService)
		return nil, err
	}

	return newVService, nil
}

func createService(req *http.Request, decoder encoding.Decoder, localClient client.Client, virtualClient client.Client, fromNamespace, targetNamespace string) (runtime.Object, error) {
	// authorization will be done at this point already, so we can redirect the request to the physical cluster
	rawObj, err := ioutil.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}

	svc, err := decoder.Decode(rawObj)
	if err != nil {
		return nil, err
	}

	vService, ok := svc.(*corev1.Service)
	if !ok {
		return nil, fmt.Errorf("expected service object")
	}

	// make sure the namespace is correct and filled
	vService.Namespace = fromNamespace

	// generate a name, because this field is cleared
	if vService.GenerateName != "" && vService.Name == "" {
		vService.Name = vService.GenerateName + random.RandomString(5)
	}

	newObj, err := translate.SetupMetadata(targetNamespace, vService)
	if err != nil {
		return nil, errors.Wrap(err, "error setting metadata")
	}

	newService := newObj.(*corev1.Service)
	newService.Spec.Selector = nil
	err = localClient.Create(req.Context(), newService)
	if err != nil {
		klog.Infof("Error creating service in physical cluster: %v", err)
		if kerrors.IsAlreadyExists(err) {
			return nil, kerrors.NewConflict(corev1.Resource("services"), vService.Name, fmt.Errorf("service %s already exists in namespace %s", vService.Name, vService.Namespace))
		}

		return nil, err
	}

	vService.Spec.ClusterIP = newService.Spec.ClusterIP
	vService.Status = newService.Status

	// now create the service in the virtual cluster
	err = virtualClient.Create(req.Context(), vService)
	if err != nil {
		// try to cleanup the created physical service
		klog.Infof("Error creating service in virtual cluster: %v", err)
		_ = localClient.Delete(context.Background(), newService)
		return nil, err
	}

	return vService, nil
}