package appdefinition

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ibuildthecloud/baaah/pkg/meta"
	"github.com/ibuildthecloud/baaah/pkg/router"
	v1 "github.com/ibuildthecloud/herd/pkg/apis/herd-project.io/v1"
	"github.com/ibuildthecloud/herd/pkg/certs"
	"github.com/ibuildthecloud/herd/pkg/condition"
	"github.com/ibuildthecloud/herd/pkg/labels"
	"github.com/pkg/errors"
	"github.com/rancher/wrangler/pkg/data/convert"
	"github.com/rancher/wrangler/pkg/randomtoken"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func seedData(from map[string]string, keys ...string) map[string][]byte {
	to := map[string][]byte{}
	for _, key := range keys {
		to[key] = []byte(from[key])
	}
	return to
}

func generateSSH(req router.Request, appInstance *v1.AppInstance, secretName string, secretRef v1.Secret) (*corev1.Secret, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: secretName + "-",
			Namespace:    appInstance.Namespace,
			Labels:       labelsForSecret(secretName, appInstance),
		},
		Data: seedData(secretRef.Data, corev1.SSHAuthPrivateKey),
		Type: corev1.SecretTypeSSHAuth,
	}

	if len(secret.Data[corev1.SSHAuthPrivateKey]) > 0 {
		return secret, req.Client.Create(secret)
	}

	params := v1.TLSParams{}
	if err := convert.ToObj(secretRef.Params, &params); err != nil {
		return nil, err
	}
	params.Complete()

	key, err := certs.GeneratePrivateKey(params.Algorithm)
	if err != nil {
		return nil, err
	}

	secret.Data[corev1.SSHAuthPrivateKey] = key
	return secret, req.Client.Create(secret)
}

func generateTLS(secrets map[string]*corev1.Secret, req router.Request, appInstance *v1.AppInstance, secretName string, secretRef v1.Secret) (*corev1.Secret, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: secretName + "-",
			Namespace:    appInstance.Namespace,
			Labels:       labelsForSecret(secretName, appInstance),
		},
		Data: seedData(secretRef.Data, corev1.TLSCertKey, corev1.TLSPrivateKeyKey, "ca.crt", "ca.key"),
		Type: corev1.SecretTypeTLS,
	}

	if len(secret.Data[corev1.TLSCertKey]) > 0 && len(secret.Data[corev1.TLSPrivateKeyKey]) > 0 {
		return secret, req.Client.Create(secret)
	}

	params := v1.TLSParams{}
	if err := convert.ToObj(secretRef.Params, &params); err != nil {
		return nil, err
	}

	var (
		err             error
		caPEM, caKeyPEM = secret.Data["ca.crt"], secret.Data["ca.key"]
	)

	if len(caPEM) == 0 || len(caKeyPEM) == 0 {
		if params.CASecret == "" {
			caPEM, caKeyPEM, err = certs.GenerateCA(params.Algorithm)
			if err != nil {
				return nil, err
			}
		} else {
			caSecret, err := getOrCreateSecret(secrets, req, appInstance, params.CASecret)
			if err != nil {
				return nil, err
			}
			caPEM, caKeyPEM = caSecret.Data["ca.crt"], caSecret.Data["ca.key"]
		}
	}

	cert, key, err := certs.GenerateCert(caPEM, caKeyPEM, params)
	if err != nil {
		return nil, err
	}

	secret.Data[corev1.TLSCertKey] = cert
	secret.Data[corev1.TLSPrivateKeyKey] = key
	if params.CASecret == "" {
		secret.Data["ca.crt"] = caPEM
		secret.Data["ca.key"] = caKeyPEM
	}

	return secret, req.Client.Create(secret)
}

func generateBasic(req router.Request, appInstance *v1.AppInstance, secretName string, secretRef v1.Secret) (*corev1.Secret, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: secretName + "-",
			Namespace:    appInstance.Namespace,
			Labels:       labelsForSecret(secretName, appInstance),
		},
		Data: seedData(secretRef.Data, corev1.BasicAuthUsernameKey, corev1.BasicAuthPasswordKey),
		Type: corev1.SecretTypeBasicAuth,
	}

	for i, key := range []string{corev1.BasicAuthUsernameKey, corev1.BasicAuthPasswordKey} {
		if len(secret.Data) == 0 {
			// TODO: Improve with more characters (special, upper/lowercase, etc)
			v, err := randomtoken.Generate()
			v = v[:(i+1)*8]
			if err != nil {
				return nil, err
			}
			secret.Data[key] = []byte(v)
		}
	}

	return secret, req.Client.Create(secret)
}

func generateDocker(req router.Request, appInstance *v1.AppInstance, name string, secretRef v1.Secret) (*corev1.Secret, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: name + "-",
			Namespace:    appInstance.Namespace,
			Labels:       labelsForSecret(name, appInstance),
		},
		Data: seedData(secretRef.Data, corev1.DockerConfigJsonKey),
		Type: corev1.SecretTypeDockerConfigJson,
	}

	if len(secret.Data[corev1.DockerConfigJsonKey]) == 0 {
		secret.Data[corev1.DockerConfigJsonKey] = []byte("{}")
	}
	return secret, req.Client.Create(secret)
}

func labelsForSecret(secretName string, appInstance *v1.AppInstance) map[string]string {
	return map[string]string{
		labels.HerdAppName:      appInstance.Name,
		labels.HerdAppNamespace: appInstance.Namespace,
		labels.HerdManaged:      "true",
		labels.HerdAppUID:       string(appInstance.UID),
		labels.HerdSecretName:   secretName,
	}
}

func getSecret(req router.Request, appInstance *v1.AppInstance, name string) (*corev1.Secret, error) {
	l := labelsForSecret(name, appInstance)

	var secrets corev1.SecretList
	err := req.Client.List(&secrets, &meta.ListOptions{
		Selector: klabels.SelectorFromSet(l),
	})
	if err != nil {
		return nil, err
	}

	if len(secrets.Items) == 0 {
		return nil, apierrors.NewNotFound(schema.GroupResource{
			Group:    "v1",
			Resource: "secrets",
		}, name)
	}

	sort.Slice(secrets.Items, func(i, j int) bool {
		return secrets.Items[i].UID < secrets.Items[j].UID
	})

	return &secrets.Items[0], nil
}

func generateSecret(secrets map[string]*corev1.Secret, req router.Request, appInstance *v1.AppInstance, secretName string) (*corev1.Secret, error) {
	secret, err := getSecret(req, appInstance, secretName)
	if apierrors.IsNotFound(err) {
		secretRef, ok := appInstance.Status.AppSpec.Secrets[secretName]
		if !ok {
			return nil, apierrors.NewNotFound(schema.GroupResource{
				Group:    "v1",
				Resource: "secrets",
			}, secretName)
		}
		switch secretRef.Type {
		case "docker":
			return generateDocker(req, appInstance, secretName, secretRef)
		case "basic":
			return generateBasic(req, appInstance, secretName, secretRef)
		case "tls":
			return generateTLS(secrets, req, appInstance, secretName, secretRef)
		case "ssh-auth":
			return generateSSH(req, appInstance, secretName, secretRef)
		default:
			return nil, err
		}
	}
	return secret, err
}

func getOrCreateSecret(secrets map[string]*corev1.Secret, req router.Request, appInstance *v1.AppInstance, secretName string) (*corev1.Secret, error) {
	if sec, ok := secrets[secretName]; ok {
		return sec, nil
	}

	for _, binding := range appInstance.Spec.Secrets {
		if binding.SecretRequest == secretName {
			existingSecret := &corev1.Secret{}
			err := req.Client.Get(existingSecret, binding.Secret, nil)
			if err != nil {
				return nil, err
			}
			secrets[secretName] = existingSecret
			return existingSecret, nil
		}
	}

	secret, err := generateSecret(secrets, req, appInstance, secretName)
	if err != nil {
		return nil, err
	}
	secrets[secretName] = secret
	return secret, nil

}

func CreateSecrets(req router.Request, resp router.Response) (err error) {
	var (
		missing     []string
		errored     []string
		appInstance = req.Object.(*v1.AppInstance)
		secrets     = map[string]*corev1.Secret{}
		cond        = condition.Setter(appInstance, resp, v1.AppInstanceConditionSecrets)
	)

	defer func() {
		if err != nil {
			cond.Error(err)
			return
		}

		sort.Strings(errored)
		buf := strings.Builder{}
		if len(missing) > 0 {
			sort.Strings(missing)
			buf.WriteString("missing: [")
			buf.WriteString(strings.Join(missing, ", "))
			buf.WriteString("]")
		}
		if len(errored) > 0 {
			sort.Strings(missing)
			if buf.Len() > 0 {
				buf.WriteString(" ")
			}
			buf.WriteString("missing: [")
			buf.WriteString(strings.Join(missing, ", "))
			buf.WriteString("]")
		}

		if buf.Len() > 0 {
			cond.Error(errors.New(buf.String()))
		} else {
			cond.Success()
		}
	}()

	for secretName, secretRef := range appInstance.Status.AppSpec.Secrets {
		secret, err := getOrCreateSecret(secrets, req, appInstance, secretName)
		if apierrors.IsNotFound(err) {
			if secretRef.Optional == nil || !*secretRef.Optional {
				missing = append(missing, secretName)
			}
			continue
		} else if apiError := apierrors.APIStatus(nil); errors.As(err, &apiError) {
			cond.Error(err)
			return err
		} else if err != nil {
			errored = append(errored, fmt.Sprintf("%s: %v", secretName, err))
			continue
		}
		resp.Objects(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: appInstance.Status.Namespace,
				Labels: map[string]string{
					labels.HerdAppName:      appInstance.Name,
					labels.HerdAppNamespace: appInstance.Namespace,
					labels.HerdManaged:      "true",
				},
			},
			Data: secret.Data,
			Type: secret.Type,
		})
	}

	return nil
}