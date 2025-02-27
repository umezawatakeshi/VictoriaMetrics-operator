package factory

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/VictoriaMetrics/operator/controllers/factory/k8stools"

	"github.com/VictoriaMetrics/operator/controllers/factory/vmagent"

	"github.com/VictoriaMetrics/operator/controllers/factory/psp"

	victoriametricsv1beta1 "github.com/VictoriaMetrics/operator/api/v1beta1"
	"github.com/VictoriaMetrics/operator/internal/config"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	vmAgentConfDir                  = "/etc/vmagent/config"
	vmAgentConOfOutDir              = "/etc/vmagent/config_out"
	vmAgentPersistentQueueDir       = "/tmp/vmagent-remotewrite-data"
	vmAgentPersistentQueueMountName = "persistent-queue-data"
)

func CreateOrUpdateVMAgentService(ctx context.Context, cr *victoriametricsv1beta1.VMAgent, rclient client.Client, c *config.BaseOperatorConf) (*corev1.Service, error) {
	l := log.WithValues("recon.vm.service.name", cr.Name)
	newService := newServiceVMAgent(cr, c)

	currentService := &corev1.Service{}
	err := rclient.Get(ctx, types.NamespacedName{Namespace: cr.Namespace, Name: newService.Name}, currentService)
	if err != nil {
		if errors.IsNotFound(err) {
			l.Info("creating new service for vmagent")
			if err := rclient.Create(ctx, newService); err != nil {
				return nil, fmt.Errorf("cannot create new service for vmagent: %w", err)
			}
			return newService, nil
		}
		return nil, fmt.Errorf("cannot get vmagent service for reconcile: %w", err)
	}
	newService.Annotations = labels.Merge(newService.Annotations, currentService.Annotations)

	if currentService.Spec.ClusterIP != "" {
		newService.Spec.ClusterIP = currentService.Spec.ClusterIP
	}
	if currentService.ResourceVersion != "" {
		newService.ResourceVersion = currentService.ResourceVersion
	}
	newService.Finalizers = victoriametricsv1beta1.MergeFinalizers(currentService, victoriametricsv1beta1.FinalizerName)
	err = rclient.Update(ctx, newService)
	if err != nil {
		l.Error(err, "cannot update vmagent service")
		return nil, err
	}
	l.Info("vmagent service reconciled")
	return newService, nil
}

func defaultVMagentService(cr *victoriametricsv1beta1.VMAgent, c *config.BaseOperatorConf) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            cr.PrefixedName(),
			Namespace:       cr.Namespace,
			Labels:          c.Labels.Merge(cr.Labels()),
			Annotations:     cr.Annotations(),
			OwnerReferences: cr.AsOwner(),
			Finalizers:      []string{victoriametricsv1beta1.FinalizerName},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: cr.SelectorLabels(),
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Protocol:   "TCP",
					Port:       intstr.Parse(c.VMAgentDefault.Port).IntVal,
					TargetPort: intstr.Parse(c.VMAgentDefault.Port),
				},
			},
		},
	}
}

func newServiceVMAgent(cr *victoriametricsv1beta1.VMAgent, c *config.BaseOperatorConf) *corev1.Service {
	cr = cr.DeepCopy()
	svc := defaultVMagentService(cr, c)
	if cr.Spec.ServiceSpec != nil {
		svc = &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:            cr.Spec.ServiceSpec.Name,
				Namespace:       cr.Namespace,
				Labels:          cr.Spec.ServiceSpec.Labels,
				Annotations:     cr.Spec.ServiceSpec.Annotations,
				OwnerReferences: cr.AsOwner(),
			},
			Spec: cr.Spec.ServiceSpec.Spec,
		}
	}
	if cr.Spec.Port != "" {
		svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{
			Protocol:   "TCP",
			Port:       intstr.Parse(cr.Spec.Port).IntVal,
			TargetPort: intstr.Parse(cr.Spec.Port),
		})
	}
	setServiceDefaultField(svc, defaultVMagentService(cr, c))
	buildAdditionalServicePorts(cr.Spec.InsertPorts, svc)
	return svc
}

// if some field of current service is not define, replace by default service
func setServiceDefaultField(cur, def *corev1.Service) {
	cur.Labels = labels.Merge(cur.Labels, def.Labels)
	cur.Annotations = labels.Merge(cur.Annotations, def.Annotations)
	if cur.Name == "" {
		cur.Name = def.Name
	}
	if cur.Spec.Type == "" {
		cur.Spec.Type = def.Spec.Type
	}
	if cur.Spec.Selector == nil {
		cur.Spec.Selector = def.Spec.Selector
	}
	if cur.Spec.Ports == nil {
		cur.Spec.Ports = def.Spec.Ports
	}
}

//we assume, that configmaps were created before this function was called
func CreateOrUpdateVMAgent(ctx context.Context, cr *victoriametricsv1beta1.VMAgent, rclient client.Client, c *config.BaseOperatorConf) (reconcile.Result, error) {
	l := log.WithValues("controller", "vmagent.crud")

	if err := psp.CreateServiceAccountForCRD(ctx, cr, rclient); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed create service account: %w", err)
	}
	if c.PSPAutoCreateEnabled {
		if err := psp.CreateOrUpdateServiceAccountWithPSP(ctx, cr, rclient); err != nil {
			l.Error(err, "cannot create podsecuritypolicy")
			return reconcile.Result{}, fmt.Errorf("cannot create podsecurity policy for vmagent, err: %w", err)
		}
	}
	if cr.GetServiceAccountName() == cr.PrefixedName() {
		l.Info("creating default clusterrole for vmagent")
		if err := vmagent.CreateVMAgentClusterAccess(ctx, cr, rclient); err != nil {
			return reconcile.Result{}, fmt.Errorf("cannot create vmagent clusterole and binding for it, err: %w", err)
		}
	}
	//we have to create empty or full cm first
	err := CreateOrUpdateConfigurationSecret(ctx, cr, rclient, c)
	if err != nil {
		l.Error(err, "cannot create configmap")
		return reconcile.Result{}, err
	}

	err = CreateOrUpdateTlsAssets(ctx, cr, rclient)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("cannot update tls asset for vmagent: %w", err)
	}

	// getting secrets for remotewrite spec
	rwsBasicAuthSecrets, rwsTokens, err := LoadRemoteWriteSecrets(ctx, cr, rclient, l)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("cannot get remote write secrets for vmagent: %w", err)
	}
	l.Info("create or update vm agent deploy")

	newDeploy, err := newDeployForVMAgent(cr, c, rwsBasicAuthSecrets, rwsTokens)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("cannot build new deploy for vmagent: %w", err)
	}

	l = l.WithValues("vmagent.deploy.name", newDeploy.Name, "vmagent.deploy.namespace", newDeploy.Namespace)

	currentDeploy := &appsv1.Deployment{}
	err = rclient.Get(ctx, types.NamespacedName{Name: newDeploy.Name, Namespace: newDeploy.Namespace}, currentDeploy)
	if err != nil {
		if errors.IsNotFound(err) {
			//create new
			l.Info("vmagent deploy not found, creating new one")
			if err := rclient.Create(ctx, newDeploy); err != nil {
				return reconcile.Result{}, fmt.Errorf("cannot create new vmagent deploy: %w", err)
			}
			l.Info("new vmagent deploy was created")
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("cannot get vmagent deploy: %s,err: %w", newDeploy.Name, err)
	}
	l.Info("updating  vmagent")
	newDeploy.Annotations = labels.Merge(newDeploy.Annotations, currentDeploy.Annotations)
	newDeploy.Spec.Template.Annotations = labels.Merge(newDeploy.Spec.Template.Annotations, currentDeploy.Spec.Template.Annotations)
	victoriametricsv1beta1.MergeFinalizers(newDeploy, victoriametricsv1beta1.FinalizerName)
	if err := rclient.Update(ctx, newDeploy); err != nil {
		return reconcile.Result{}, err
	}

	//its safe to ignore
	_ = addAddtionalScrapeConfigOwnership(cr, rclient, l)
	l.Info("vmagent deploy reconciled")

	return reconcile.Result{}, nil
}

// newDeployForVMAgent builds vmagent deployment spec.
func newDeployForVMAgent(cr *victoriametricsv1beta1.VMAgent, c *config.BaseOperatorConf, rwsBasicAuth map[string]BasicAuthCredentials, rwsTokens map[string]BearerToken) (*appsv1.Deployment, error) {
	cr = cr.DeepCopy()

	//inject default
	if cr.Spec.Image.Repository == "" {
		cr.Spec.Image.Repository = c.VMAgentDefault.Image
	}
	if cr.Spec.Image.Tag == "" {
		cr.Spec.Image.Tag = c.VMAgentDefault.Version
	}
	if cr.Spec.Image.PullPolicy == "" {
		cr.Spec.Image.PullPolicy = corev1.PullIfNotPresent
	}

	if cr.Spec.Port == "" {
		cr.Spec.Port = c.VMAgentDefault.Port
	}

	podSpec, err := makeSpecForVMAgent(cr, c, rwsBasicAuth, rwsTokens)
	if err != nil {
		return nil, err
	}

	depSpec := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            cr.PrefixedName(),
			Namespace:       cr.Namespace,
			Labels:          c.Labels.Merge(cr.Labels()),
			Annotations:     cr.Annotations(),
			OwnerReferences: cr.AsOwner(),
			Finalizers:      []string{victoriametricsv1beta1.FinalizerName},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: cr.Spec.ReplicaCount,
			Selector: &metav1.LabelSelector{
				MatchLabels: cr.SelectorLabels(),
			},
			Strategy: appsv1.DeploymentStrategy{
				Type:          appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{},
			},
			Template: *podSpec,
		},
	}
	return depSpec, nil
}

func makeSpecForVMAgent(cr *victoriametricsv1beta1.VMAgent, c *config.BaseOperatorConf, rwsBasicAuth map[string]BasicAuthCredentials, rwsTokens map[string]BearerToken) (*corev1.PodTemplateSpec, error) {
	args := []string{
		fmt.Sprintf("-promscrape.config=%s", path.Join(vmAgentConOfOutDir, configEnvsubstFilename)),
		fmt.Sprintf("-remoteWrite.tmpDataPath=%s", vmAgentPersistentQueueDir),
		// limit to 1GB
		"-remoteWrite.maxDiskUsagePerURL=1073741824",
	}

	if len(cr.Spec.RemoteWrite) > 0 {
		args = append(args, BuildRemoteWrites(cr, rwsBasicAuth, rwsTokens)...)
	}
	args = append(args, BuildRemoteWriteSettings(cr)...)

	for arg, value := range cr.Spec.ExtraArgs {
		args = append(args, fmt.Sprintf("-%s=%s", arg, value))
	}

	args = append(args, fmt.Sprintf("-httpListenAddr=:%s", cr.Spec.Port))

	if cr.Spec.LogLevel != "" {
		args = append(args, fmt.Sprintf("-loggerLevel=%s", cr.Spec.LogLevel))
	}
	if cr.Spec.LogFormat != "" {
		args = append(args, fmt.Sprintf("-loggerFormat=%s", cr.Spec.LogFormat))
	}
	if len(cr.Spec.ExtraEnvs) > 0 {
		args = append(args, "-envflag.enable=true")
	}

	var envs []corev1.EnvVar

	envs = append(envs, cr.Spec.ExtraEnvs...)

	var ports []corev1.ContainerPort
	ports = append(ports, corev1.ContainerPort{Name: "http", Protocol: "TCP", ContainerPort: intstr.Parse(cr.Spec.Port).IntVal})
	ports = buildAdditionalContainerPorts(ports, cr.Spec.InsertPorts)

	var volumes []corev1.Volume
	volumes = append(volumes, corev1.Volume{
		Name: vmAgentPersistentQueueMountName,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	})
	volumes = append(volumes, cr.Spec.Volumes...)
	volumes = append(volumes, corev1.Volume{
		Name: "config",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: cr.PrefixedName(),
			},
		},
	},
		corev1.Volume{
			Name: "tls-assets",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: cr.TLSAssetName(),
				},
			},
		},
		corev1.Volume{
			Name: "config-out",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
	)

	var agentVolumeMounts []corev1.VolumeMount

	agentVolumeMounts = append(agentVolumeMounts,
		corev1.VolumeMount{
			Name:      vmAgentPersistentQueueMountName,
			MountPath: vmAgentPersistentQueueDir,
		},
	)
	agentVolumeMounts = append(agentVolumeMounts, cr.Spec.VolumeMounts...)
	agentVolumeMounts = append(agentVolumeMounts,
		corev1.VolumeMount{
			Name:      "config-out",
			ReadOnly:  true,
			MountPath: vmAgentConOfOutDir,
		},
		corev1.VolumeMount{
			Name:      "tls-assets",
			ReadOnly:  true,
			MountPath: tlsAssetsDir,
		},
	)

	for _, s := range cr.Spec.Secrets {
		volumes = append(volumes, corev1.Volume{
			Name: k8stools.SanitizeVolumeName("secret-" + s),
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: s,
				},
			},
		})
		agentVolumeMounts = append(agentVolumeMounts, corev1.VolumeMount{
			Name:      k8stools.SanitizeVolumeName("secret-" + s),
			ReadOnly:  true,
			MountPath: path.Join(SecretsDir, s),
		})
	}

	for _, c := range cr.Spec.ConfigMaps {
		volumes = append(volumes, corev1.Volume{
			Name: k8stools.SanitizeVolumeName("configmap-" + c),
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: c,
					},
				},
			},
		})
		agentVolumeMounts = append(agentVolumeMounts, corev1.VolumeMount{
			Name:      k8stools.SanitizeVolumeName("configmap-" + c),
			ReadOnly:  true,
			MountPath: path.Join(ConfigMapsDir, c),
		})
	}

	configReloadVolumeMounts := []corev1.VolumeMount{
		{
			Name:      "config",
			MountPath: vmAgentConfDir,
		},
		{
			Name:      "config-out",
			MountPath: vmAgentConOfOutDir,
		},
	}

	configReloadArgs := []string{
		fmt.Sprintf("--reload-url=%s", cr.ReloadPathWithPort(cr.Spec.Port)),
		fmt.Sprintf("--config-file=%s", path.Join(vmAgentConfDir, configFilename)),
		fmt.Sprintf("--config-envsubst-file=%s", path.Join(vmAgentConOfOutDir, configEnvsubstFilename)),
	}

	if cr.Spec.RelabelConfig != nil {
		volumes = append(volumes, corev1.Volume{
			Name: k8stools.SanitizeVolumeName("configmap-" + cr.Spec.RelabelConfig.Name),
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: cr.Spec.RelabelConfig.Name,
					},
				},
			},
		})
		agentVolumeMounts = append(agentVolumeMounts, corev1.VolumeMount{
			Name:      k8stools.SanitizeVolumeName("configmap-" + cr.Spec.RelabelConfig.Name),
			ReadOnly:  true,
			MountPath: path.Join(ConfigMapsDir, cr.Spec.RelabelConfig.Name),
		})

		args = append(args, "-remoteWrite.relabelConfig="+path.Join(ConfigMapsDir, cr.Spec.RelabelConfig.Name, cr.Spec.RelabelConfig.Key))
	}

	args = buildArgsForAdditionalPorts(args, cr.Spec.InsertPorts)

	for _, rw := range cr.Spec.RemoteWrite {
		if rw.UrlRelabelConfig == nil {
			continue
		}
		if cr.Spec.RelabelConfig != nil && rw.UrlRelabelConfig.Name == cr.Spec.RelabelConfig.Name {
			continue
		}
		volumes = append(volumes, corev1.Volume{
			Name: k8stools.SanitizeVolumeName("configmap-" + rw.UrlRelabelConfig.Name),
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: rw.UrlRelabelConfig.Name,
					},
				},
			},
		})
		agentVolumeMounts = append(agentVolumeMounts, corev1.VolumeMount{
			Name:      k8stools.SanitizeVolumeName("configmap-" + rw.UrlRelabelConfig.Name),
			ReadOnly:  true,
			MountPath: path.Join(ConfigMapsDir, rw.UrlRelabelConfig.Name),
		})
	}

	specRes := buildResources(cr.Spec.Resources, config.Resource(c.VMAgentDefault.Resource), c.VMAgentDefault.UseDefaultResources)

	livenessProbeHandler := corev1.Handler{
		HTTPGet: &corev1.HTTPGetAction{
			Port:   intstr.Parse(cr.Spec.Port),
			Scheme: "HTTP",
			Path:   cr.HealthPath(),
		},
	}
	readinessProbeHandler := corev1.Handler{
		HTTPGet: &corev1.HTTPGetAction{
			Port:   intstr.Parse(cr.Spec.Port),
			Scheme: "HTTP",
			Path:   cr.HealthPath(),
		},
	}
	livenessFailureThreshold := int32(3)
	livenessProbe := &corev1.Probe{
		Handler:          livenessProbeHandler,
		PeriodSeconds:    5,
		TimeoutSeconds:   probeTimeoutSeconds,
		FailureThreshold: livenessFailureThreshold,
	}
	readinessProbe := &corev1.Probe{
		Handler:          readinessProbeHandler,
		TimeoutSeconds:   probeTimeoutSeconds,
		PeriodSeconds:    5,
		FailureThreshold: 10,
	}

	var additionalContainers []corev1.Container

	prometheusConfigReloaderResources := corev1.ResourceRequirements{
		Limits: corev1.ResourceList{}, Requests: corev1.ResourceList{}}
	if c.VMAgentDefault.ConfigReloaderCPU != "0" && c.VMAgentDefault.UseDefaultResources {
		prometheusConfigReloaderResources.Limits[corev1.ResourceCPU] = resource.MustParse(c.VMAgentDefault.ConfigReloaderCPU)
	}
	if c.VMAgentDefault.ConfigReloaderMemory != "0" && c.VMAgentDefault.UseDefaultResources {
		prometheusConfigReloaderResources.Limits[corev1.ResourceMemory] = resource.MustParse(c.VMAgentDefault.ConfigReloaderMemory)
	}

	sort.Strings(args)
	operatorContainers := append([]corev1.Container{
		{
			Name:                     "config-reloader",
			Image:                    c.VMAgentDefault.ConfigReloadImage,
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			Env: []corev1.EnvVar{
				{
					Name: "POD_NAME",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
					},
				},
			},
			Command:      []string{"/bin/prometheus-config-reloader"},
			Args:         configReloadArgs,
			VolumeMounts: configReloadVolumeMounts,
			Resources:    prometheusConfigReloaderResources,
		},
		{
			Name:                     "vmagent",
			Image:                    fmt.Sprintf("%s:%s", cr.Spec.Image.Repository, cr.Spec.Image.Tag),
			ImagePullPolicy:          cr.Spec.Image.PullPolicy,
			Ports:                    ports,
			Args:                     args,
			Env:                      envs,
			VolumeMounts:             agentVolumeMounts,
			LivenessProbe:            livenessProbe,
			ReadinessProbe:           readinessProbe,
			Resources:                specRes,
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		},
	}, additionalContainers...)

	containers, err := k8stools.MergePatchContainers(operatorContainers, cr.Spec.Containers)
	if err != nil {
		return nil, err
	}

	vmAgentSpec := &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      cr.PodLabels(),
			Annotations: cr.PodAnnotations(),
		},
		Spec: corev1.PodSpec{
			Volumes:                   volumes,
			InitContainers:            cr.Spec.InitContainers,
			Containers:                containers,
			ServiceAccountName:        cr.GetServiceAccountName(),
			SecurityContext:           cr.Spec.SecurityContext,
			ImagePullSecrets:          cr.Spec.ImagePullSecrets,
			Affinity:                  cr.Spec.Affinity,
			SchedulerName:             cr.Spec.SchedulerName,
			Tolerations:               cr.Spec.Tolerations,
			PriorityClassName:         cr.Spec.PriorityClassName,
			HostNetwork:               cr.Spec.HostNetwork,
			DNSPolicy:                 cr.Spec.DNSPolicy,
			RuntimeClassName:          cr.Spec.RuntimeClassName,
			HostAliases:               cr.Spec.HostAliases,
			TopologySpreadConstraints: cr.Spec.TopologySpreadConstraints,
		},
	}

	return vmAgentSpec, nil
}

//add ownership - it needs for object changing tracking
func addAddtionalScrapeConfigOwnership(cr *victoriametricsv1beta1.VMAgent, rclient client.Client, l logr.Logger) error {
	if cr.Spec.AdditionalScrapeConfigs == nil {
		return nil
	}
	secret := &corev1.Secret{}
	err := rclient.Get(context.Background(), types.NamespacedName{Namespace: cr.Namespace, Name: cr.Spec.AdditionalScrapeConfigs.Name}, secret)
	if err != nil {
		return fmt.Errorf("cannot get secret with additional scrape config: %s, err: %w ", cr.Spec.AdditionalScrapeConfigs.Name, err)
	}
	for _, owner := range secret.OwnerReferences {
		//owner exists
		if owner.Name == cr.Name {
			return nil
		}
	}
	secret.OwnerReferences = append(secret.OwnerReferences, metav1.OwnerReference{
		APIVersion:         cr.APIVersion,
		Kind:               cr.Kind,
		Name:               cr.Name,
		Controller:         pointer.BoolPtr(false),
		BlockOwnerDeletion: pointer.BoolPtr(false),
		UID:                cr.UID,
	})

	l.Info("updating additional scrape secret ownership", "secret", secret.Name)
	err = rclient.Update(context.Background(), secret)
	if err != nil {
		return fmt.Errorf("cannot update secret ownership for vmagent Additional scrape: %s, err: %w", secret.Name, err)
	}
	l.Info("scrape secret was updated with new owner", "secret", secret.Name)
	return nil
}

func CreateOrUpdateTlsAssets(ctx context.Context, cr *victoriametricsv1beta1.VMAgent, rclient client.Client) error {
	podScrapes, err := SelectPodScrapes(ctx, cr, rclient)
	if err != nil {
		return fmt.Errorf("cannot select PodScrapes: %w", err)
	}
	scrapes, err := SelectServiceScrapes(ctx, cr, rclient)
	if err != nil {
		return fmt.Errorf("cannot select service scrapes for tls Assets: %w", err)
	}
	assets, err := loadTLSAssets(ctx, rclient, cr, scrapes, podScrapes)
	if err != nil {
		return fmt.Errorf("cannot load tls assets: %w", err)
	}

	tlsAssetsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            cr.TLSAssetName(),
			Labels:          cr.Labels(),
			OwnerReferences: cr.AsOwner(),
			Namespace:       cr.Namespace,
			Finalizers:      []string{victoriametricsv1beta1.FinalizerName},
		},
		Data: map[string][]byte{},
	}

	for key, asset := range assets {
		tlsAssetsSecret.Data[key] = []byte(asset)
	}
	currentAssetSecret := &corev1.Secret{}
	err = rclient.Get(ctx, types.NamespacedName{Namespace: cr.Namespace, Name: tlsAssetsSecret.Name}, currentAssetSecret)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("creating new tls asset for vmagent", "secret_name", tlsAssetsSecret.Name, "vmagent", cr.Name)
			return rclient.Create(ctx, tlsAssetsSecret)
		}
		return fmt.Errorf("cannot get existing tls secret: %s, for vmagent: %s, err: %w", tlsAssetsSecret.Name, cr.Name, err)
	}
	tlsAssetsSecret.Annotations = labels.Merge(tlsAssetsSecret.Annotations, currentAssetSecret.Annotations)
	victoriametricsv1beta1.MergeFinalizers(tlsAssetsSecret, victoriametricsv1beta1.FinalizerName)
	return rclient.Update(ctx, tlsAssetsSecret)
}

func loadTLSAssets(ctx context.Context, rclient client.Client, cr *victoriametricsv1beta1.VMAgent, scrapes map[string]*victoriametricsv1beta1.VMServiceScrape, podScrapes map[string]*victoriametricsv1beta1.VMPodScrape) (map[string]string, error) {
	assets := map[string]string{}
	nsSecretCache := make(map[string]*corev1.Secret)
	nsConfigMapCache := make(map[string]*corev1.ConfigMap)

	for _, rw := range cr.Spec.RemoteWrite {
		if rw.TLSConfig == nil {
			continue
		}
		if err := addAssetsToCache(ctx, rclient, cr.Namespace, rw.TLSConfig, assets, nsSecretCache, nsConfigMapCache); err != nil {
			return nil, fmt.Errorf("cannot add asset for remote write target: %s", cr.Name)
		}
	}
	for _, pod := range podScrapes {
		for _, ep := range pod.Spec.PodMetricsEndpoints {
			if ep.TLSConfig == nil {
				continue
			}
			if err := addAssetsToCache(ctx, rclient, pod.Namespace, ep.TLSConfig, assets, nsSecretCache, nsConfigMapCache); err != nil {
				return nil, fmt.Errorf("cannot add asset for vmpodscrape: %s", pod.Name)
			}
		}
	}
	for _, mon := range scrapes {
		for _, ep := range mon.Spec.Endpoints {
			if ep.TLSConfig == nil {
				continue
			}
			if err := addAssetsToCache(ctx, rclient, mon.Namespace, ep.TLSConfig, assets, nsSecretCache, nsConfigMapCache); err != nil {
				return nil, fmt.Errorf("cannot add asset for vmservicescrape: %s", mon.Name)
			}
		}
	}

	return assets, nil
}

func addAssetsToCache(
	ctx context.Context,
	rclient client.Client,
	objectNS string,
	tlsConfig *victoriametricsv1beta1.TLSConfig,
	assets map[string]string,
	nsSecretCache map[string]*corev1.Secret,
	nsConfigMapCache map[string]*corev1.ConfigMap,
) error {
	prefix := objectNS + "/"
	secretSelectors := map[string]*corev1.SecretKeySelector{}
	configMapSelectors := map[string]*corev1.ConfigMapKeySelector{}
	if tlsConfig.CA != (victoriametricsv1beta1.SecretOrConfigMap{}) {
		selectorKey := tlsConfig.CA.BuildSelectorWithPrefix(prefix)
		switch {
		case tlsConfig.CA.Secret != nil:
			secretSelectors[selectorKey] = tlsConfig.CA.Secret
		case tlsConfig.CA.ConfigMap != nil:
			configMapSelectors[selectorKey] = tlsConfig.CA.ConfigMap
		}
	}
	if tlsConfig.Cert != (victoriametricsv1beta1.SecretOrConfigMap{}) {
		selectorKey := tlsConfig.Cert.BuildSelectorWithPrefix(prefix)
		switch {
		case tlsConfig.Cert.Secret != nil:
			secretSelectors[selectorKey] = tlsConfig.Cert.Secret
		case tlsConfig.Cert.ConfigMap != nil:
			configMapSelectors[selectorKey] = tlsConfig.Cert.ConfigMap
		}
	}
	if tlsConfig.KeySecret != nil {
		secretSelectors[prefix+tlsConfig.KeySecret.Name+"/"+tlsConfig.KeySecret.Key] = tlsConfig.KeySecret
	}

	for key, selector := range secretSelectors {
		asset, err := getCredFromSecret(
			ctx,
			rclient,
			objectNS,
			*selector,
			key,
			nsSecretCache,
		)
		if err != nil {
			return fmt.Errorf(
				"failed to extract endpoint tls asset  from secret %s and key %s in namespace %s",
				selector.Name, selector.Key, objectNS,
			)
		}

		assets[tlsConfig.BuildAssetPath(objectNS, selector.Name, selector.Key)] = asset
	}

	for key, selector := range configMapSelectors {
		asset, err := getCredFromConfigMap(
			ctx,
			rclient,
			objectNS,
			*selector,
			key,
			nsConfigMapCache,
		)
		if err != nil {
			return fmt.Errorf(
				"failed to extract endpoint tls asset from configmap %s and key %s in namespace %s",
				selector.Name, selector.Key, objectNS,
			)
		}

		assets[tlsConfig.BuildAssetPath(objectNS, selector.Name, selector.Key)] = asset
	}
	return nil

}

func LoadRemoteWriteSecrets(ctx context.Context, cr *victoriametricsv1beta1.VMAgent, rclient client.Client, l logr.Logger) (map[string]BasicAuthCredentials, map[string]BearerToken, error) {
	SecretsInNS := &corev1.SecretList{}
	err := rclient.List(ctx, SecretsInNS)
	if err != nil {
		l.Error(err, "cannot list secrets at vmagent namespace")
		return nil, nil, err
	}
	rwsBasicAuthSecrets, err := loadBasicAuthSecrets(ctx, rclient, nil, nil, nil, nil, nil, cr.Spec.RemoteWrite, SecretsInNS)
	if err != nil {
		l.Error(err, "cannot load basic auth secrets for remote write specs")
		return nil, nil, err
	}

	rwsBearerTokens, err := loadBearerTokensFromSecrets(ctx, rclient, nil, nil, nil, nil, cr.Spec.RemoteWrite, SecretsInNS)
	if err != nil {
		l.Error(err, "cannot get bearer tokens for remote write specs")
		return nil, nil, err
	}
	return rwsBasicAuthSecrets, rwsBearerTokens, nil
}

type remoteFlag struct {
	isNotNull   bool
	flagSetting string
}

func BuildRemoteWriteSettings(cr *victoriametricsv1beta1.VMAgent) []string {
	if cr.Spec.RemoteWriteSettings == nil {
		return nil
	}
	var args []string
	rws := *cr.Spec.RemoteWriteSettings
	if rws.FlushInterval != nil {
		args = append(args, fmt.Sprintf("-remoteWrite.flushInterval=%s", *rws.FlushInterval))
	}
	if rws.MaxBlockSize != nil {
		args = append(args, fmt.Sprintf("-remoteWrite.maxBlockSize=%d", *rws.MaxBlockSize))
	}
	if rws.MaxDiskUsagePerURL != nil {
		args = append(args, fmt.Sprintf("-remoteWrite.maxDiskUsagePerURL=%d", *rws.MaxDiskUsagePerURL))
	}
	if rws.Queues != nil {
		args = append(args, fmt.Sprintf("-remoteWrite.queues=%d", *rws.Queues))
	}
	if rws.ShowURL != nil {
		args = append(args, fmt.Sprintf("-remoteWrite.showURL=%t", *rws.ShowURL))
	}
	if rws.TmpDataPath != nil {
		args = append(args, fmt.Sprintf("-remoteWrite.tmpDataPath=%s", *rws.TmpDataPath))
	}
	return args
}

func BuildRemoteWrites(cr *victoriametricsv1beta1.VMAgent, rwsBasicAuth map[string]BasicAuthCredentials, rwsTokens map[string]BearerToken) []string {
	var finalArgs []string
	var remoteArgs []remoteFlag
	remoteTargets := cr.Spec.RemoteWrite

	url := remoteFlag{flagSetting: "-remoteWrite.url=", isNotNull: true}
	authUser := remoteFlag{flagSetting: "-remoteWrite.basicAuth.username="}
	authPassword := remoteFlag{flagSetting: "-remoteWrite.basicAuth.password="}
	bearerToken := remoteFlag{flagSetting: "-remoteWrite.bearerToken="}
	labels := remoteFlag{flagSetting: "-remoteWrite.label="}
	urlRelabelConfig := remoteFlag{flagSetting: "-remoteWrite.urlRelabelConfig="}
	sendTimeout := remoteFlag{flagSetting: "-remoteWrite.sendTimeout="}
	tlsCAs := remoteFlag{flagSetting: "-remoteWrite.tlsCAFile="}
	tlsCerts := remoteFlag{flagSetting: "-remoteWrite.tlsCertFile="}
	tlsKeys := remoteFlag{flagSetting: "-remoteWrite.tlsKeyFile="}
	tlsInsecure := remoteFlag{flagSetting: "-remoteWrite.tlsInsecureSkipVerify="}
	tlsServerName := remoteFlag{flagSetting: "-remoteWrite.tlsServerName="}

	pathPrefix := path.Join(tlsAssetsDir, cr.Namespace)

	for _, rws := range remoteTargets {

		url.flagSetting += fmt.Sprintf("%s,", rws.URL)

		var caPath, certPath, keyPath, ServerName string
		var insecure bool
		if rws.TLSConfig != nil {
			if rws.TLSConfig.CAFile != "" {
				caPath = rws.TLSConfig.CAFile
			} else {
				caPath = rws.TLSConfig.BuildAssetPath(pathPrefix, rws.TLSConfig.CA.Name(), rws.TLSConfig.CA.Key())
			}
			tlsCAs.isNotNull = true
			if rws.TLSConfig.CertFile != "" {
				certPath = rws.TLSConfig.CertFile
			} else {
				certPath = rws.TLSConfig.BuildAssetPath(pathPrefix, rws.TLSConfig.Cert.Name(), rws.TLSConfig.Cert.Key())

			}
			tlsCerts.isNotNull = true
			if rws.TLSConfig.KeyFile != "" {
				keyPath = rws.TLSConfig.KeyFile
			} else {
				keyPath = rws.TLSConfig.BuildAssetPath(pathPrefix, rws.TLSConfig.KeySecret.Name, rws.TLSConfig.KeySecret.Key)
			}
			tlsKeys.isNotNull = true
			if rws.TLSConfig.InsecureSkipVerify {
				tlsInsecure.isNotNull = true
			}
			if rws.TLSConfig.ServerName != "" {
				ServerName = rws.TLSConfig.ServerName
				tlsServerName.isNotNull = true
			}
			insecure = rws.TLSConfig.InsecureSkipVerify
		}
		tlsCAs.flagSetting += fmt.Sprintf("%s,", caPath)
		tlsCerts.flagSetting += fmt.Sprintf("%s,", certPath)
		tlsKeys.flagSetting += fmt.Sprintf("%s,", keyPath)
		tlsServerName.flagSetting += fmt.Sprintf("%s,", ServerName)
		tlsInsecure.flagSetting += fmt.Sprintf("%v,", insecure)

		var user string
		var pass string
		if rws.BasicAuth != nil {
			if s, ok := rwsBasicAuth[fmt.Sprintf("remoteWriteSpec/%s", rws.URL)]; ok {
				authUser.isNotNull = true
				authPassword.isNotNull = true
				user = s.username
				pass = s.password
			}
		}
		authUser.flagSetting += fmt.Sprintf("\"%s\",", strings.Replace(user, `"`, `\"`, -1))
		authPassword.flagSetting += fmt.Sprintf("\"%s\",", strings.Replace(pass, `"`, `\"`, -1))

		var value string
		if rws.BearerTokenSecret != nil {
			if s, ok := rwsTokens[fmt.Sprintf("remoteWriteSpec/%s", rws.URL)]; ok {
				bearerToken.isNotNull = true
				value = string(s)
			}
		}
		bearerToken.flagSetting += fmt.Sprintf("\"%s\",", strings.Replace(value, `"`, `\"`, -1))

		value = ""
		if rws.Labels != nil {
			labels.isNotNull = true
			labels := sortMap(rws.Labels)
			for _, v := range labels {
				value += fmt.Sprintf("%s=%s,", v.key, v.value)
			}
		}
		// no need to add comma
		labels.flagSetting += value

		value = ""

		if rws.UrlRelabelConfig != nil {
			urlRelabelConfig.isNotNull = true
			value = path.Join(ConfigMapsDir, rws.UrlRelabelConfig.Name, rws.UrlRelabelConfig.Key)
		}
		urlRelabelConfig.flagSetting += fmt.Sprintf("%s,", value)

		value = ""
		if rws.SendTimeout != nil {
			if !sendTimeout.isNotNull {
				sendTimeout.isNotNull = true
				finalArgs = append(finalArgs, fmt.Sprintf("%s=%s", sendTimeout.flagSetting, *rws.SendTimeout))
			}
			value = *rws.SendTimeout
		}
		sendTimeout.flagSetting += fmt.Sprintf("%s,", value)

		value = ""
	}
	remoteArgs = append(remoteArgs, url, authUser, authPassword, bearerToken, labels, urlRelabelConfig, tlsInsecure, sendTimeout)
	remoteArgs = append(remoteArgs, tlsServerName, tlsKeys, tlsCerts, tlsCAs)

	for _, remoteArgType := range remoteArgs {
		if remoteArgType.isNotNull {
			finalArgs = append(finalArgs, strings.TrimSuffix(remoteArgType.flagSetting, ","))
		}
	}
	return finalArgs
}
