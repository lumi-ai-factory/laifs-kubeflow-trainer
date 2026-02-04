// Package remote implements a custom Kubeflow Trainer runtime plugin that
// offloads training execution outside Kubernetes (e.g. to an HPC system).
//
// The plugin extracts the inline Python script produced by the Trainer SDK,
// stores it in a ConfigMap, and patches the generated JobSet so that the
// trainer pod runs a lightweight runner which executes the script remotely.
//
// JobSet templates are modified via reflection instead of importing JobSet
// API types directly. This keeps the plugin decoupled from specific JobSet
// versions and apply-configuration layouts.
//
// Optionally, a SLURM endpoint/URI can be passed to the runner via a TrainJob
// annotation and injected as an environment variable.

package remote

import (
	"context"
	"fmt"
	"reflect"

	apiruntime "k8s.io/apimachinery/pkg/runtime"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	trainer "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	"github.com/kubeflow/trainer/v2/pkg/runtime"
	"github.com/kubeflow/trainer/v2/pkg/runtime/framework"
)

const (
	Name               = "remote"
	ScriptKey          = "script.py"
	SlurmURIAnnotation = "remote.trainer.kubeflow.org/slurm-uri"

	ScriptVolumeName = "remote-script"
	ScriptMountDir   = "/mnt/remote"
	ScriptFilePath   = "/mnt/remote/script.py"

	TrainerContainerName = "node"
)

type Remote struct {
	client client.Client
}

var _ framework.ComponentBuilderPlugin = (*Remote)(nil)

func New(_ context.Context, c client.Client, _ client.FieldIndexer) (framework.Plugin, error) {
	return &Remote{client: c}, nil
}

func (r *Remote) Name() string {
	return Name
}

// Build is invoked by the Trainer runtime during reconciliation.
// It extracts the inline training script, persists it as a ConfigMap,
// and mutates the JobSet apply-configuration so that the trainer pod
// executes a remote runner instead of running training in-cluster.
func (r *Remote) Build(
	ctx context.Context,
	info *runtime.Info,
	job *trainer.TrainJob,
) ([]apiruntime.ApplyConfiguration, error) {

	if info == nil || job == nil || job.Spec.Trainer == nil {
		return nil, nil
	}

	if len(job.Spec.Trainer.Command) < 3 {
		return nil, fmt.Errorf(
			"remote-runtime: expected trainer.command[2] to contain python script, got: %v",
			job.Spec.Trainer.Command,
		)
	}

	// 1) Extract inline python "script" the Trainer SDK put into trainer.command[2]
	script := job.Spec.Trainer.Command[2]
	cmName := fmt.Sprintf("%s-remote-script", job.Name)

	// 2) Create ConfigMap with script.py
	cm := corev1ac.ConfigMap(cmName, job.Namespace).
		WithData(map[string]string{ScriptKey: script})

	// 3) Optional slurm URI from TrainJob annotation
	slurmURI := ""
	if job.Annotations != nil {
		slurmURI = job.Annotations[SlurmURIAnnotation]
	}

	// Debug: what ObjApply is
	fmt.Printf("REMOTE: ObjApply type=%T\n", info.TemplateSpec.ObjApply)
	v := reflect.ValueOf(info.TemplateSpec.ObjApply)
	kind := "<invalid>"
	if v.IsValid() {
		kind = v.Kind().String()
	}
	fmt.Printf("REMOTE: ObjApply kind=%s\n", kind)
	fmt.Printf("REMOTE: TrainJob=%s/%s\n", job.Namespace, job.Name)

	// 4) Patch ObjApply (source of truth) so the pod runs our runner + mounts script CM
	patched := patchRunnerInObjApply(info.TemplateSpec.ObjApply, cmName, slurmURI)
	fmt.Printf("REMOTE: patched=%v slurmURI=%q cm=%q\n", patched, slurmURI, cmName)

	return []apiruntime.ApplyConfiguration{cm}, nil
}

// patchRunnerInObjApply patches the JobSet ApplyConfiguration stored in ObjApply
// without importing JobSet types.
// It:
// - finds replicatedJobs[].template.spec.template.spec (PodSpecApplyConfiguration)
// - locates container named "node"
// - sets Command = ["python3", "/app/firecrest_runner.py"] and clears Args
// - ensures script ConfigMap is mounted to /app and SCRIPT_PATH=/app/script.py is set
// - upserts env SLURM_URI if provided
func patchRunnerInObjApply(obj any, cmName string, slurmURI string) bool {
	v := reflect.ValueOf(obj)
	if !v.IsValid() {
		return false
	}
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return false
		}
		v = v.Elem()
	}

	// IMPORTANT:
	// ObjApply is currently *v1alpha2.JobSetSpecApplyConfiguration, where ReplicatedJobs is
	// at the top-level: ObjApply.ReplicatedJobs (NOT ObjApply.Spec.ReplicatedJobs).
	// But keep a fallback for other shapes.
	rjobs := findReplicatedJobsField(v)
	if !rjobs.IsValid() || rjobs.Kind() != reflect.Slice {
		return false
	}

	patched := false

	for i := 0; i < rjobs.Len(); i++ {
		rj := rjobs.Index(i)
		if rj.Kind() == reflect.Ptr {
			if rj.IsNil() {
				continue
			}
			rj = rj.Elem()
		}

		// replicatedJobs[i].Template
		templ := rj.FieldByName("Template")
		if !templ.IsValid() {
			continue
		}
		if templ.Kind() == reflect.Ptr {
			if templ.IsNil() {
				continue
			}
			templ = templ.Elem()
		}

		// Template.Spec
		templSpec := templ.FieldByName("Spec")
		if !templSpec.IsValid() {
			continue
		}
		if templSpec.Kind() == reflect.Ptr {
			if templSpec.IsNil() {
				continue
			}
			templSpec = templSpec.Elem()
		}

		// Spec.Template
		podTemplate := templSpec.FieldByName("Template")
		if !podTemplate.IsValid() {
			continue
		}
		if podTemplate.Kind() == reflect.Ptr {
			if podTemplate.IsNil() {
				continue
			}
			podTemplate = podTemplate.Elem()
		}

		// Template.Spec
		podTemplateSpec := podTemplate.FieldByName("Spec")
		if !podTemplateSpec.IsValid() {
			continue
		}
		if podTemplateSpec.Kind() == reflect.Ptr {
			if podTemplateSpec.IsNil() {
				continue
			}
			podTemplateSpec = podTemplateSpec.Elem()
		}

		// Ensure podSpec.Volumes has CM volume
		upsertVolume(podTemplateSpec, ScriptVolumeName, cmName)

		containers := podTemplateSpec.FieldByName("Containers")
		if !containers.IsValid() || containers.Kind() != reflect.Slice {
			continue
		}

		for j := 0; j < containers.Len(); j++ {
			c := containers.Index(j)
			if c.Kind() == reflect.Ptr {
				if c.IsNil() {
					continue
				}
				c = c.Elem()
			}

			if getStringField(c.FieldByName("Name")) != TrainerContainerName {
				continue
			}

			// Set Command
			cmd := c.FieldByName("Command")
			if cmd.IsValid() && cmd.CanSet() && cmd.Kind() == reflect.Slice {
				cmd.Set(makeStringSlice(cmd.Type(), []string{"python3", "/runner/firecrest_runner.py"}))
			}

			// Clear Args
			args := c.FieldByName("Args")
			if args.IsValid() && args.CanSet() && args.Kind() == reflect.Slice {
				args.Set(reflect.Zero(args.Type()))
			}

			// Ensure mount + SCRIPT_PATH
			upsertVolumeMount(c, ScriptVolumeName, ScriptMountDir)
			upsertEnvVar(c, "SCRIPT_PATH", ScriptFilePath)

			// Optional SLURM_URI
			if slurmURI != "" {
				upsertEnvVar(c, "SLURM_URI", slurmURI)
			}

			patched = true
		}
	}

	return patched
}

// Helpers
func findReplicatedJobsField(v reflect.Value) reflect.Value {
	// 1) direct
	rj := v.FieldByName("ReplicatedJobs")
	if rj.IsValid() {
		return rj
	}

	// 2) nested under Spec
	spec := v.FieldByName("Spec")
	if !spec.IsValid() {
		return reflect.Value{}
	}
	if spec.Kind() == reflect.Ptr {
		if spec.IsNil() {
			return reflect.Value{}
		}
		spec = spec.Elem()
	}
	rj = spec.FieldByName("ReplicatedJobs")
	if rj.IsValid() {
		return rj
	}

	return reflect.Value{}
}

func getStringField(f reflect.Value) string {
	if !f.IsValid() {
		return ""
	}
	if f.Kind() == reflect.String {
		return f.String()
	}
	if f.Kind() == reflect.Ptr && !f.IsNil() && f.Elem().Kind() == reflect.String {
		return f.Elem().String()
	}
	return ""
}

func makeStringSlice(sliceType reflect.Type, vals []string) reflect.Value {
	// sliceType is something like []string OR []*string
	elemT := sliceType.Elem()
	out := reflect.MakeSlice(sliceType, 0, len(vals))

	for _, s := range vals {
		out = reflect.Append(out, makeStringElem(elemT, s))
	}
	return out
}

func makeStringElem(elemT reflect.Type, s string) reflect.Value {
	// elemT is string OR *string
	if elemT.Kind() == reflect.String {
		return reflect.ValueOf(s).Convert(elemT)
	}
	if elemT.Kind() == reflect.Ptr && elemT.Elem().Kind() == reflect.String {
		p := reflect.New(elemT.Elem())
		p.Elem().SetString(s)
		return p.Convert(elemT)
	}
	// fallback: try direct convert (may panic if incompatible)
	return reflect.ValueOf(s).Convert(elemT)
}

func upsertEnvVar(container reflect.Value, name string, value string) {
	env := container.FieldByName("Env")
	if !env.IsValid() || !env.CanSet() || env.Kind() != reflect.Slice {
		return
	}

	// Find existing
	for i := 0; i < env.Len(); i++ {
		ev := env.Index(i)
		if ev.Kind() == reflect.Ptr {
			if ev.IsNil() {
				continue
			}
			ev = ev.Elem()
		}

		if getStringField(ev.FieldByName("Name")) == name {
			vf := ev.FieldByName("Value")
			if vf.IsValid() && vf.CanSet() {
				if vf.Kind() == reflect.String {
					vf.SetString(value)
				} else if vf.Kind() == reflect.Ptr && vf.Type().Elem().Kind() == reflect.String {
					p := reflect.New(vf.Type().Elem())
					p.Elem().SetString(value)
					vf.Set(p)
				}
			}
			return
		}
	}

	// Append new env var using applyconfig helper (handles pointer fields nicely)
	cfg := corev1ac.EnvVar().WithName(name).WithValue(value)
	appendApplyConfig(env, cfg)
}

func upsertVolume(podSpec reflect.Value, volName string, cmName string) {
	vols := podSpec.FieldByName("Volumes")
	if !vols.IsValid() || !vols.CanSet() || vols.Kind() != reflect.Slice {
		return
	}

	// If exists, done
	for i := 0; i < vols.Len(); i++ {
		v := vols.Index(i)
		if v.Kind() == reflect.Ptr {
			if v.IsNil() {
				continue
			}
			v = v.Elem()
		}
		if getStringField(v.FieldByName("Name")) == volName {
			return
		}
	}

	cfg := corev1ac.Volume().
		WithName(volName).
		WithConfigMap(corev1ac.ConfigMapVolumeSource().WithName(cmName))

	appendApplyConfig(vols, cfg)
}

func upsertVolumeMount(container reflect.Value, volName string, mountPath string) {
	vms := container.FieldByName("VolumeMounts")
	if !vms.IsValid() || !vms.CanSet() || vms.Kind() != reflect.Slice {
		return
	}

	// If exists, done (or ensure mountPath)
	for i := 0; i < vms.Len(); i++ {
		vm := vms.Index(i)
		if vm.Kind() == reflect.Ptr {
			if vm.IsNil() {
				continue
			}
			vm = vm.Elem()
		}
		if getStringField(vm.FieldByName("Name")) == volName {
			// ensure mountPath
			mp := vm.FieldByName("MountPath")
			if mp.IsValid() && mp.CanSet() {
				if mp.Kind() == reflect.String {
					mp.SetString(mountPath)
				} else if mp.Kind() == reflect.Ptr && mp.Type().Elem().Kind() == reflect.String {
					p := reflect.New(mp.Type().Elem())
					p.Elem().SetString(mountPath)
					mp.Set(p)
				}
			}
			return
		}
	}

	cfg := corev1ac.VolumeMount().WithName(volName).WithMountPath(mountPath)
	appendApplyConfig(vms, cfg)
}

// appendApplyConfig appends *ApplyConfiguration to a slice field that may be either:
// - []T  (struct apply config)
// - []*T (pointer apply config)
func appendApplyConfig(sliceVal reflect.Value, cfg any) {
	cfgV := reflect.ValueOf(cfg) // usually *T
	if !cfgV.IsValid() {
		return
	}

	elemT := sliceVal.Type().Elem()

	// Case 1: slice is []*T
	if elemT.Kind() == reflect.Ptr {
		if cfgV.Type().AssignableTo(elemT) {
			sliceVal.Set(reflect.Append(sliceVal, cfgV))
			return
		}
		// If cfg is T and slice wants *T, take address if possible
		if cfgV.Kind() == reflect.Struct && reflect.PtrTo(cfgV.Type()).AssignableTo(elemT) {
			p := reflect.New(cfgV.Type())
			p.Elem().Set(cfgV)
			sliceVal.Set(reflect.Append(sliceVal, p.Convert(elemT)))
			return
		}
	}

	// Case 2: slice is []T (struct)
	if elemT.Kind() == reflect.Struct {
		// cfg is *T -> deref
		if cfgV.Kind() == reflect.Ptr && !cfgV.IsNil() && cfgV.Elem().Type().AssignableTo(elemT) {
			sliceVal.Set(reflect.Append(sliceVal, cfgV.Elem().Convert(elemT)))
			return
		}
		// cfg is T
		if cfgV.Type().AssignableTo(elemT) {
			sliceVal.Set(reflect.Append(sliceVal, cfgV.Convert(elemT)))
			return
		}
	}
}
