package tracejob

import (
	"fmt"
	"io"
	"io/ioutil"

	"github.com/iovisor/kubectl-trace/pkg/meta"
	batchv1 "k8s.io/api/batch/v1"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	batchv1typed "k8s.io/client-go/kubernetes/typed/batch/v1"
	corev1typed "k8s.io/client-go/kubernetes/typed/core/v1"
)

type TraceJobClient struct {
	JobClient    batchv1typed.JobInterface
	ConfigClient corev1typed.ConfigMapInterface
	outStream    io.Writer
}

// TraceJob is a container of info needed to create the job responsible for tracing.
type TraceJob struct {
	Name             string
	ID               types.UID
	Namespace        string
	ServiceAccount   string
	Hostname         string
	Program          string
	PodUID           string
	ContainerName    string
	IsPod            bool
	ImageNameTag     string
	InitImageNameTag string
	FetchHeaders     bool
	StartTime        metav1.Time
	Status           TraceJobStatus
}

// WithOutStream setup a file stream to output trace job operation information
func (t *TraceJobClient) WithOutStream(o io.Writer) {
	if o == nil {
		t.outStream = ioutil.Discard
	}
	t.outStream = o
}

type TraceJobFilter struct {
	Name *string
	ID   *types.UID
}

func (nf TraceJobFilter) selectorOptions() metav1.ListOptions {
	selectorOptions := metav1.ListOptions{}

	if nf.Name != nil {
		selectorOptions = metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s", meta.TraceLabelKey, *nf.Name),
		}
	}

	if nf.ID != nil {
		selectorOptions = metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s", meta.TraceIDLabelKey, *nf.ID),
		}
	}

	if nf.Name == nil && nf.ID == nil {
		selectorOptions = metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s", meta.TraceIDLabelKey),
		}
	}

	return selectorOptions
}

func (t *TraceJobClient) findJobsWithFilter(nf TraceJobFilter) ([]batchv1.Job, error) {

	selectorOptions := nf.selectorOptions()
	if len(selectorOptions.LabelSelector) == 0 {
		return []batchv1.Job{}, nil
	}

	jl, err := t.JobClient.List(selectorOptions)

	if err != nil {
		return nil, err
	}
	return jl.Items, nil
}

func (t *TraceJobClient) findConfigMapsWithFilter(nf TraceJobFilter) ([]apiv1.ConfigMap, error) {
	selectorOptions := nf.selectorOptions()
	if len(selectorOptions.LabelSelector) == 0 {
		return []apiv1.ConfigMap{}, nil
	}

	cm, err := t.ConfigClient.List(selectorOptions)

	if err != nil {
		return nil, err
	}
	return cm.Items, nil
}

func (t *TraceJobClient) GetJob(nf TraceJobFilter) ([]TraceJob, error) {
	jl, err := t.findJobsWithFilter(nf)
	if err != nil {
		return nil, err
	}
	tjobs := []TraceJob{}

	for _, j := range jl {
		labels := j.GetLabels()
		name, ok := labels[meta.TraceLabelKey]
		if !ok {
			name = ""
		}
		id, ok := labels[meta.TraceIDLabelKey]
		if !ok {
			id = ""
		}
		hostname, err := jobHostname(j)
		if err != nil {
			hostname = ""
		}
		tj := TraceJob{
			Name:      name,
			ID:        types.UID(id),
			Namespace: j.Namespace,
			Hostname:  hostname,
			StartTime: *j.Status.StartTime,
			Status:    jobStatus(j),
		}
		tjobs = append(tjobs, tj)
	}

	return tjobs, nil
}

func (t *TraceJobClient) DeleteJobs(nf TraceJobFilter) error {
	nothingDeleted := true
	jl, err := t.findJobsWithFilter(nf)
	if err != nil {
		return err
	}

	dp := metav1.DeletePropagationForeground
	for _, j := range jl {
		err := t.JobClient.Delete(j.Name, &metav1.DeleteOptions{
			GracePeriodSeconds: int64Ptr(0),
			PropagationPolicy:  &dp,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(t.outStream, "trace job %s deleted\n", j.Name)
		nothingDeleted = false
	}

	cl, err := t.findConfigMapsWithFilter(nf)

	if err != nil {
		return err
	}

	for _, c := range cl {
		err := t.ConfigClient.Delete(c.Name, nil)
		if err != nil {
			return err
		}
		fmt.Fprintf(t.outStream, "trace configuration %s deleted\n", c.Name)
		nothingDeleted = false
	}

	if nothingDeleted {
		fmt.Fprintf(t.outStream, "error: no trace found to be deleted\n")
	}
	return nil
}

func (t *TraceJobClient) CreateJob(nj TraceJob) (*batchv1.Job, error) {

	bpfTraceCmd := []string{
		"/bin/trace-runner",
		"--program=/programs/program.bt",
	}

	if nj.IsPod {
		bpfTraceCmd = append(bpfTraceCmd, "--inpod")
		bpfTraceCmd = append(bpfTraceCmd, "--container="+nj.ContainerName)
		bpfTraceCmd = append(bpfTraceCmd, "--poduid="+nj.PodUID)
	}

	commonMeta := metav1.ObjectMeta{
		Name:      nj.Name,
		Namespace: nj.Namespace,
		Labels: map[string]string{
			meta.TraceLabelKey:   nj.Name,
			meta.TraceIDLabelKey: string(nj.ID),
		},
		Annotations: map[string]string{
			meta.TraceLabelKey:   nj.Name,
			meta.TraceIDLabelKey: string(nj.ID),
		},
	}

	cm := &apiv1.ConfigMap{
		ObjectMeta: commonMeta,
		Data: map[string]string{
			"program.bt": nj.Program,
		},
	}

	job := &batchv1.Job{
		ObjectMeta: commonMeta,
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: int32Ptr(5),
			Parallelism:             int32Ptr(1),
			Completions:             int32Ptr(1),
			BackoffLimit:            int32Ptr(1),
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: commonMeta,
				Spec: apiv1.PodSpec{
					HostPID:            true,
					ServiceAccountName: nj.ServiceAccount,
					Volumes: []apiv1.Volume{
						apiv1.Volume{
							Name: "program",
							VolumeSource: apiv1.VolumeSource{
								ConfigMap: &apiv1.ConfigMapVolumeSource{
									LocalObjectReference: apiv1.LocalObjectReference{
										Name: cm.Name,
									},
								},
							},
						},
						apiv1.Volume{
							Name: "modules",
							VolumeSource: apiv1.VolumeSource{
								HostPath: &apiv1.HostPathVolumeSource{
									Path: "/lib/modules",
								},
							},
						},
						apiv1.Volume{
							Name: "sys",
							VolumeSource: apiv1.VolumeSource{
								HostPath: &apiv1.HostPathVolumeSource{
									Path: "/sys",
								},
							},
						},
					},
					Containers: []apiv1.Container{
						apiv1.Container{
							Name:    nj.Name,
							Image:   nj.ImageNameTag,
							Command: bpfTraceCmd,
							TTY:     true,
							Stdin:   true,
							Resources: apiv1.ResourceRequirements{
								Requests: apiv1.ResourceList{
									apiv1.ResourceCPU:    resource.MustParse("100m"),
									apiv1.ResourceMemory: resource.MustParse("100Mi"),
								},
								Limits: apiv1.ResourceList{
									apiv1.ResourceCPU:    resource.MustParse("1"),
									apiv1.ResourceMemory: resource.MustParse("1G"),
								},
							},
							VolumeMounts: []apiv1.VolumeMount{
								apiv1.VolumeMount{
									Name:      "program",
									MountPath: "/programs",
									ReadOnly:  true,
								},
								apiv1.VolumeMount{
									Name:      "modules",
									MountPath: "/lib/modules",
									ReadOnly:  true,
								},
								apiv1.VolumeMount{
									Name:      "sys",
									MountPath: "/sys",
									ReadOnly:  true,
								},
							},
							SecurityContext: &apiv1.SecurityContext{
								Privileged: boolPtr(true),
							},
						},
					},
					RestartPolicy: "Never",
					Affinity: &apiv1.Affinity{
						NodeAffinity: &apiv1.NodeAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: &apiv1.NodeSelector{
								NodeSelectorTerms: []apiv1.NodeSelectorTerm{
									apiv1.NodeSelectorTerm{
										MatchExpressions: []apiv1.NodeSelectorRequirement{
											apiv1.NodeSelectorRequirement{
												Key:      "kubernetes.io/hostname",
												Operator: apiv1.NodeSelectorOpIn,
												Values:   []string{nj.Hostname},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	if _, err := t.ConfigClient.Create(cm); err != nil {
		return nil, err
	}
	return t.JobClient.Create(job)
}

func int32Ptr(i int32) *int32 { return &i }
func int64Ptr(i int64) *int64 { return &i }
func boolPtr(b bool) *bool    { return &b }

func jobHostname(j batchv1.Job) (string, error) {
	aff := j.Spec.Template.Spec.Affinity
	if aff == nil {
		return "", fmt.Errorf("affinity not found for job")
	}

	nodeAff := aff.NodeAffinity

	if nodeAff == nil {
		return "", fmt.Errorf("node affinity not found for job")
	}

	requiredScheduling := nodeAff.RequiredDuringSchedulingIgnoredDuringExecution

	if requiredScheduling == nil {
		return "", fmt.Errorf("node affinity RequiredDuringSchedulingIgnoredDuringExecution not found for job")
	}
	nst := requiredScheduling.NodeSelectorTerms
	if len(nst) == 0 {
		return "", fmt.Errorf("node selector terms are empty in node affinity for job")
	}

	me := nst[0].MatchExpressions

	if len(me) == 0 {
		return "", fmt.Errorf("node selector terms match expressions are empty in node affinity for job")
	}

	for _, v := range me {
		if v.Key == "kubernetes.io/hostname" {
			if len(v.Values) == 0 {
				return "", fmt.Errorf("hostname affinity found but no values in it for job")
			}

			return v.Values[0], nil
		}
	}

	return "", fmt.Errorf("hostname not found for job")
}

// TraceJobStatus is a label for the running status of a trace job at the current time.
type TraceJobStatus string

// These are the valid status of traces.
const (
	// TraceJobRunning means the trace job has active pods.
	TraceJobRunning TraceJobStatus = "Running"
	// TraceJobCompleted means the trace job does not have any active pod and has success pods.
	TraceJobCompleted TraceJobStatus = "Completed"
	// TraceJobFailed means the trace job does not have any active or success pod and has fpods that failed.
	TraceJobFailed TraceJobStatus = "Failed"
	// TraceJobUnknown means that for some reason we do not have the information to determine the status.
	TraceJobUnknown TraceJobStatus = "Unknown"
)

func jobStatus(j batchv1.Job) TraceJobStatus {
	if j.Status.Active > 0 {
		return TraceJobRunning
	}
	if j.Status.Succeeded > 0 {
		return TraceJobCompleted
	}
	if j.Status.Failed > 0 {
		return TraceJobFailed
	}
	return TraceJobUnknown
}