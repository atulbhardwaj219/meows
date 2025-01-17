package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	constants "github.com/cybozu-go/meows"
	meowsv1alpha1 "github.com/cybozu-go/meows/api/v1alpha1"
	"github.com/cybozu-go/meows/github"
	"github.com/cybozu-go/meows/runner"
	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// RunnerPoolReconciler reconciles a RunnerPool object
type RunnerPoolReconciler struct {
	client.Client
	log                logr.Logger
	scheme             *runtime.Scheme
	runnerImage        string
	runnerManager      RunnerManager
	secretUpdater      SecretUpdater
	organizationRegexp *regexp.Regexp
	repositoryRegexp   *regexp.Regexp
}

// NewRunnerPoolReconciler creates RunnerPoolReconciler
func NewRunnerPoolReconciler(
	log logr.Logger, client client.Client, scheme *runtime.Scheme, runnerImage string,
	runnerManager RunnerManager, secretUpdater SecretUpdater,
	organizationRegexp, repositoryRegexp *regexp.Regexp) *RunnerPoolReconciler {
	return &RunnerPoolReconciler{
		Client:             client,
		log:                log.WithName("RunnerPool"),
		scheme:             scheme,
		runnerImage:        runnerImage,
		runnerManager:      runnerManager,
		secretUpdater:      secretUpdater,
		organizationRegexp: organizationRegexp,
		repositoryRegexp:   repositoryRegexp,
	}
}

//+kubebuilder:rbac:groups=meows.cybozu.com,resources=runnerpools,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=meows.cybozu.com,resources=runnerpools/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *RunnerPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.log.WithValues("runnerpool", req.NamespacedName)
	log.Info("start reconciliation loop")

	rp := &meowsv1alpha1.RunnerPool{}
	if err := r.Get(ctx, req.NamespacedName, rp); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("runnerpool is not found")
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to get RunnerPool")
		return ctrl.Result{}, err
	}

	if rp.ObjectMeta.DeletionTimestamp != nil {
		if !controllerutil.ContainsFinalizer(rp, constants.RunnerPoolFinalizer) {
			return ctrl.Result{}, nil
		}

		log.Info("start finalizing RunnerPool")

		if err := r.runnerManager.Stop(rp); err != nil {
			log.Error(err, "failed to stop runner manager")
			return ctrl.Result{}, err
		}

		if err := r.secretUpdater.Stop(rp); err != nil {
			log.Error(err, "failed to stop secret updater")
			return ctrl.Result{}, err
		}

		controllerutil.RemoveFinalizer(rp, constants.RunnerPoolFinalizer)
		if err := r.Update(ctx, rp); err != nil {
			log.Error(err, "failed to remove finalizer")
			return ctrl.Result{}, err
		}

		log.Info("finalizing RunnerPool is completed")
		return ctrl.Result{}, nil
	}

	err := r.validation(ctx, rp)
	if err != nil {
		log.Error(err, "validation error")
		return ctrl.Result{}, err
	}

	cred, err := r.getGitHubCredential(ctx, log, rp)
	if err != nil {
		log.Error(err, "failed to get github credential")
		return ctrl.Result{}, err
	}

	isContinuation, err := r.reconcileSecret(ctx, log, rp)
	if err != nil {
		log.Error(err, "failed to reconcile secret")
		return ctrl.Result{}, err
	}
	if err := r.secretUpdater.Start(rp, cred); err != nil {
		log.Error(err, "failed to start secret updater")
		return ctrl.Result{}, err
	}
	if !isContinuation {
		log.Info("wait for the secret to be issued by secret updater")
		return ctrl.Result{
			Requeue:      true,
			RequeueAfter: 10 * time.Second,
		}, nil
	}

	if err := r.reconcileDeployment(ctx, log, rp); err != nil {
		log.Error(err, "failed to reconcile deployment")
		return ctrl.Result{}, err
	}

	if err := r.runnerManager.StartOrUpdate(rp, cred); err != nil {
		log.Error(err, "failed to start or update runner manager")
		return ctrl.Result{}, err
	}

	rp.Status.Bound = true
	if err := r.Status().Update(ctx, rp); err != nil {
		log.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *RunnerPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&meowsv1alpha1.RunnerPool{}).
		Owns(&corev1.Secret{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}

func labelSet(rp *meowsv1alpha1.RunnerPool) map[string]string {
	labels := map[string]string{
		constants.AppNameLabelKey:      constants.AppName,
		constants.AppComponentLabelKey: constants.AppComponentRunner,
		constants.AppInstanceLabelKey:  rp.Name,
	}
	return labels
}

func mergeMap(m1, m2 map[string]string) map[string]string {
	m := make(map[string]string)
	for k, v := range m1 {
		m[k] = v
	}
	for k, v := range m2 {
		m[k] = v
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

func readAppKeySecret(s *corev1.Secret) (*github.ClientCredential, error) {
	appIDstr, ok := s.Data[constants.CredentialSecretDataAppID]
	if !ok {
		return nil, fmt.Errorf("missing %s key", constants.CredentialSecretDataAppID)
	}
	appID, err := strconv.Atoi(string(appIDstr))
	if err != nil {
		return nil, fmt.Errorf("invalid %s value; %w", constants.CredentialSecretDataAppID, err)
	}

	insIDstr, ok := s.Data[constants.CredentialSecretDataAppInstallationID]
	if !ok {
		return nil, fmt.Errorf("missing %s key", constants.CredentialSecretDataAppInstallationID)
	}
	insID, err := strconv.Atoi(string(insIDstr))
	if err != nil {
		return nil, fmt.Errorf("invalid %s value; %w", constants.CredentialSecretDataAppInstallationID, err)
	}

	key, ok := s.Data[constants.CredentialSecretDataAppPrivateKey]
	if !ok {
		return nil, fmt.Errorf("missing %s key", constants.CredentialSecretDataAppPrivateKey)
	}

	return &github.ClientCredential{
		AppID:             int64(appID),
		AppInstallationID: int64(insID),
		PrivateKey:        key,
	}, nil
}

func (r *RunnerPoolReconciler) getGitHubCredential(ctx context.Context, log logr.Logger, rp *meowsv1alpha1.RunnerPool) (*github.ClientCredential, error) {
	secretName := constants.DefaultCredentialSecretName
	if rp.Spec.CredentialSecretName != "" {
		secretName = rp.Spec.CredentialSecretName
	}

	s := &corev1.Secret{}
	err := r.Client.Get(ctx, types.NamespacedName{
		Name:      secretName,
		Namespace: rp.Namespace,
	}, s)
	if err != nil {
		return nil, fmt.Errorf("failed to get credential secret; %w", err)
	}

	if pat, ok := s.Data[constants.CredentialSecretDataPATToken]; ok {
		return &github.ClientCredential{
			PersonalAccessToken: string(pat),
		}, nil
	}

	return readAppKeySecret(s)
}

func (r *RunnerPoolReconciler) validation(ctx context.Context, rp *meowsv1alpha1.RunnerPool) error {
	if rp.IsOrgLevel() {
		if r.organizationRegexp != nil && !r.organizationRegexp.MatchString(rp.Spec.Organization) {
			return errors.New("organization is not match")
		}
	} else {
		if r.repositoryRegexp != nil && !r.repositoryRegexp.MatchString(rp.Spec.Repository) {
			return errors.New("repository is not match")
		}
	}
	return nil
}

func (r *RunnerPoolReconciler) reconcileSecret(ctx context.Context, log logr.Logger, rp *meowsv1alpha1.RunnerPool) (bool, error) {
	s := &corev1.Secret{}
	err := r.Client.Get(ctx, types.NamespacedName{
		Name:      rp.GetRunnerSecretName(),
		Namespace: rp.Namespace,
	}, s)
	if err == nil {
		if _, ok := s.Annotations[constants.RunnerSecretExpiresAtAnnotationKey]; ok {
			return true, nil
		}
		return false, nil
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}

	s.SetName(rp.GetRunnerSecretName())
	s.SetNamespace(rp.Namespace)
	if err := ctrl.SetControllerReference(rp, s, r.scheme); err != nil {
		return false, err
	}
	return false, r.Create(ctx, s)
}

func (r *RunnerPoolReconciler) reconcileDeployment(ctx context.Context, log logr.Logger, rp *meowsv1alpha1.RunnerPool) error {
	d := &appsv1.Deployment{}
	d.SetNamespace(rp.GetNamespace())
	d.SetName(rp.GetRunnerDeploymentName())

	var orig, updated *appsv1.DeploymentSpec
	op, err := ctrl.CreateOrUpdate(ctx, r.Client, d, func() error {
		orig = d.Spec.DeepCopy()

		d.Labels = mergeMap(d.GetLabels(), labelSet(rp))
		d.Spec.Selector = &metav1.LabelSelector{MatchLabels: labelSet(rp)}

		d.Spec.Template.Labels = mergeMap(d.Spec.Template.GetLabels(), rp.Spec.Template.ObjectMeta.Labels)
		d.Spec.Template.Labels = mergeMap(d.Spec.Template.GetLabels(), labelSet(rp))
		d.Spec.Template.Annotations = mergeMap(d.Spec.Template.GetAnnotations(), rp.Spec.Template.ObjectMeta.Annotations)

		d.Spec.Replicas = pointer.Int32Ptr(rp.Spec.Replicas)
		d.Spec.Template.Spec.ServiceAccountName = rp.Spec.Template.ServiceAccountName
		d.Spec.Template.Spec.ImagePullSecrets = rp.Spec.Template.ImagePullSecrets
		if rp.Spec.Template.AutomountServiceAccountToken != nil {
			d.Spec.Template.Spec.AutomountServiceAccountToken = rp.Spec.Template.AutomountServiceAccountToken
		}

		varDir := "var-dir"
		workDir := "work-dir"
		volumes := append(rp.Spec.Template.Volumes, corev1.Volume{
			Name: varDir,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		})
		if rp.Spec.WorkVolume == nil {
			// use emptyDir (default)
			volumes = append(volumes, corev1.Volume{
				Name: workDir,
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			})
		} else {
			volumes = append(volumes, corev1.Volume{
				Name:         workDir,
				VolumeSource: *rp.Spec.WorkVolume,
			})
		}

		volumes = append(volumes, corev1.Volume{
			Name: rp.GetRunnerSecretName(),
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: rp.GetRunnerSecretName(),
				},
			},
		})
		d.Spec.Template.Spec.Volumes = volumes

		r.addRunnerContainerIfNotExists(d)
		runnerContainer := r.findRunnerContainer(d)

		// Update the runner container.
		if rp.Spec.Template.RunnerContainer.Image != "" {
			runnerContainer.Image = rp.Spec.Template.RunnerContainer.Image
		} else {
			runnerContainer.Image = r.runnerImage
		}
		if rp.Spec.Template.RunnerContainer.ImagePullPolicy != "" {
			runnerContainer.ImagePullPolicy = rp.Spec.Template.RunnerContainer.ImagePullPolicy
		}
		runnerContainer.SecurityContext = rp.Spec.Template.RunnerContainer.SecurityContext
		runnerContainer.Resources = rp.Spec.Template.RunnerContainer.Resources
		runnerContainer.Ports = r.makeRunnerContainerPorts()

		volumeMounts := append(rp.Spec.Template.RunnerContainer.VolumeMounts, corev1.VolumeMount{
			Name:      varDir,
			MountPath: constants.RunnerVarDirPath,
		}, corev1.VolumeMount{
			Name:      workDir,
			MountPath: constants.RunnerWorkDirPath,
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      rp.GetRunnerSecretName(),
			ReadOnly:  true,
			MountPath: filepath.Join(constants.RunnerVarDirPath, constants.SecretsDirName),
		})
		runnerContainer.VolumeMounts = volumeMounts

		env, err := r.makeRunnerContainerEnv(rp)
		if err != nil {
			return err
		}
		runnerContainer.Env = env

		updated = d.Spec.DeepCopy()
		return ctrl.SetControllerReference(rp, d, r.scheme)
	})

	if err != nil {
		log.Error(err, "failed to reconcile deployment")
		return err
	}
	switch op {
	case controllerutil.OperationResultCreated:
		log.Info("reconciled deployment", "operation", string(op))
	case controllerutil.OperationResultUpdated:
		// The deployment update should occur only when the users update their RunnerPool CR.
		// If this log shows frequently, users need to review their RunnerPool CR.
		log.Info("reconciled deployment", "operation", string(op), "diff", cmp.Diff(orig, updated))
	}
	return nil
}

func (r *RunnerPoolReconciler) findRunnerContainer(d *appsv1.Deployment) *corev1.Container {
	for i := range d.Spec.Template.Spec.Containers {
		c := &d.Spec.Template.Spec.Containers[i]
		if c.Name == constants.RunnerContainerName {
			return c
		}
	}
	return nil
}

func (r *RunnerPoolReconciler) addRunnerContainerIfNotExists(d *appsv1.Deployment) {
	if c := r.findRunnerContainer(d); c != nil {
		// When the runner container already exists, nothing to do.
		return
	}

	// Create the runner container.
	c := corev1.Container{
		Name: constants.RunnerContainerName,
	}
	d.Spec.Template.Spec.Containers = append(d.Spec.Template.Spec.Containers, c)
}

func (r *RunnerPoolReconciler) makeRunnerContainerEnv(rp *meowsv1alpha1.RunnerPool) ([]corev1.EnvVar, error) {
	option := runner.Option{
		SetupCommand: rp.Spec.SetupCommand,
	}
	optionJson, err := json.Marshal(&option)
	if err != nil {
		return nil, err
	}

	envs := []corev1.EnvVar{
		{
			Name: constants.PodNameEnvName,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.name",
				},
			},
		},
		{
			Name: constants.PodNamespaceEnvName,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.namespace",
				},
			},
		},
		{
			Name:  constants.RunnerPoolNameEnvName,
			Value: rp.ObjectMeta.Name,
		},
		{
			Name:  constants.RunnerOptionEnvName,
			Value: string(optionJson),
		},
	}

	if rp.IsOrgLevel() {
		envs = append(envs, corev1.EnvVar{
			Name:  constants.RunnerOrgEnvName,
			Value: rp.Spec.Organization,
		})
	} else {
		envs = append(envs, corev1.EnvVar{
			Name:  constants.RunnerRepoEnvName,
			Value: rp.Spec.Repository,
		})
	}

	// NOTE:
	// We need not ignore the reserved environment variables here.
	// Since the reserved environment variables are checked in the validating webhook.
	envs = append(envs, rp.Spec.Template.RunnerContainer.Env...)

	return envs, nil
}

func (r *RunnerPoolReconciler) makeRunnerContainerPorts() []corev1.ContainerPort {
	return []corev1.ContainerPort{
		{
			Protocol:      corev1.ProtocolTCP,
			Name:          constants.RunnerMetricsPortName,
			ContainerPort: constants.RunnerListenPort,
		},
	}
}
