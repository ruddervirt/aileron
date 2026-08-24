package build

import (
	"context"
	"errors"
	"testing"
	"time"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestCPUCores(t *testing.T) {
	cases := []struct {
		cpu  string
		want int64
	}{
		{"0", 1},     // defensive floor
		{"100m", 1},  // fractional rounds up to 1
		{"0.1", 1},   // same, decimal form
		{"500m", 1},  // still 1 core
		{"1", 1},     // exact
		{"1.5", 2},   // rounds up
		{"2", 2},     // exact
		{"2.5", 3},   // rounds up
		{"4", 4},     // exact
		{"4000m", 4}, // milli form
	}
	for _, c := range cases {
		got := cpuCores(resource.MustParse(c.cpu))
		if got != c.want {
			t.Errorf("cpuCores(%q) = %d, want %d", c.cpu, got, c.want)
		}
	}
}

func buildForInvisibleTest(invisible bool) *v1alpha1.VirtualMachineBuild {
	return &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "invistest", Namespace: "ruddervirt-system"},
		Spec: v1alpha1.VirtualMachineBuildSpec{
			VMs: []v1alpha1.BuildVM{{
				Name:      "webserver",
				Source:    v1alpha1.BuildSource{Blank: true},
				Invisible: invisible,
			}},
		},
		Status: v1alpha1.VirtualMachineBuildStatus{
			BuildID:        "vm-test1",
			BuildNamespace: "ruddervirt-system",
		},
	}
}

func TestBuildVMStampsInvisibleAnnotation(t *testing.T) {
	build := buildForInvisibleTest(true)
	vm, err := (&VMBooter{}).buildVM(build, &build.Spec.VMs[0])
	if err != nil {
		t.Fatalf("buildVM: %v", err)
	}
	annotations, _, _ := unstructured.NestedStringMap(vm.Object, "metadata", "annotations")
	if got := annotations[v1alpha1.AnnotationInvisible]; got != valueTrue {
		t.Errorf("metadata.annotations[%s] = %q, want %q", v1alpha1.AnnotationInvisible, got, valueTrue)
	}
}

func TestBuildVMOmitsInvisibleAnnotationWhenFalse(t *testing.T) {
	build := buildForInvisibleTest(false)
	vm, err := (&VMBooter{}).buildVM(build, &build.Spec.VMs[0])
	if err != nil {
		t.Fatalf("buildVM: %v", err)
	}
	annotations, _, _ := unstructured.NestedStringMap(vm.Object, "metadata", "annotations")
	if _, ok := annotations[v1alpha1.AnnotationInvisible]; ok {
		t.Errorf("metadata.annotations[%s] should be absent, got %q", v1alpha1.AnnotationInvisible, annotations[v1alpha1.AnnotationInvisible])
	}
}

// synchronizedFalseCondition builds a status.conditions entry matching what
// KubeVirt sets when it can't sync a VMI/VM yet, e.g. "PVC pending" while a
// rootdisk clone is still copying.
func synchronizedFalseCondition(msg string, lastTransitionTime *time.Time) map[string]any {
	cond := map[string]any{
		"type":    "Synchronized",
		"status":  "False",
		"message": msg,
	}
	if lastTransitionTime != nil {
		cond["lastTransitionTime"] = lastTransitionTime.Format(time.RFC3339)
	}
	return cond
}

func TestSyncFailureFromConditions_RecentTransitionReturnsThatTime(t *testing.T) {
	since := time.Now().Add(-30 * time.Second)
	obj := map[string]any{
		"status": map[string]any{
			"conditions": []any{synchronizedFalseCondition("PVC pending", &since)},
		},
	}
	state, msg, got := syncFailureFromConditions(obj, "VMI")
	if state != vmiFailed {
		t.Fatalf("state = %v, want vmiFailed", state)
	}
	if msg != "VMI Synchronized=False: PVC pending" {
		t.Errorf("msg = %q", msg)
	}
	// RFC3339 truncates sub-second precision, so allow either direction.
	if diff := got.Sub(since); diff < -time.Second || diff > time.Second {
		t.Errorf("since = %v, want ~%v", got, since)
	}
}

func TestSyncFailureFromConditions_MissingTimestampDefaultsToNow(t *testing.T) {
	obj := map[string]any{
		"status": map[string]any{
			"conditions": []any{synchronizedFalseCondition("PVC pending", nil)},
		},
	}
	before := time.Now()
	state, _, got := syncFailureFromConditions(obj, "VMI")
	after := time.Now()
	if state != vmiFailed {
		t.Fatalf("state = %v, want vmiFailed", state)
	}
	if got.Before(before) || got.After(after) {
		t.Errorf("since = %v, want between %v and %v", got, before, after)
	}
}

func TestSyncFailureFromConditions_NotFailedWhenTrueOrEmptyMessage(t *testing.T) {
	trueCond := map[string]any{"type": "Synchronized", "status": "True", "message": ""}
	emptyMsgCond := map[string]any{"type": "Synchronized", "status": "False", "message": ""}
	for name, obj := range map[string]map[string]any{
		"status-true":   {"status": map[string]any{"conditions": []any{trueCond}}},
		"empty-message": {"status": map[string]any{"conditions": []any{emptyMsgCond}}},
		"no-conditions": {"status": map[string]any{}},
	} {
		t.Run(name, func(t *testing.T) {
			state, msg, since := syncFailureFromConditions(obj, "VMI")
			if state != vmiPending || msg != "" || !since.IsZero() {
				t.Errorf("got (%v, %q, %v), want (vmiPending, \"\", zero)", state, msg, since)
			}
		})
	}
}

var vmiGVK = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstance"}

// vmiScheme extends vmScheme (defined in template_test.go) with the
// VirtualMachineInstance kind, since checkVMI fetches both.
func vmiScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := vmScheme(t)
	s.AddKnownTypeWithName(vmiGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{
		Group: vmiGVK.Group, Version: vmiGVK.Version, Kind: vmiGVK.Kind + "List",
	}, &unstructured.UnstructuredList{})
	return s
}

func vmiWithSyncFailure(name, namespace, msg string, since time.Time) *unstructured.Unstructured {
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(vmiGVK)
	vmi.SetName(name)
	vmi.SetNamespace(namespace)
	_ = unstructured.SetNestedSlice(vmi.Object, []any{synchronizedFalseCondition(msg, &since)}, "status", "conditions")
	return vmi
}

func TestCheckVMI_SynchronizedFalseWithinGracePeriod_StaysPending(t *testing.T) {
	vmi := vmiWithSyncFailure("workstation1", "vm-test1", "PVC pending", time.Now().Add(-30*time.Second))
	cl := fake.NewClientBuilder().WithScheme(vmiScheme(t)).WithObjects(vmi).Build()
	v := &VMBooter{Client: cl}

	state, msg, err := v.checkVMI(context.Background(), "workstation1", "vm-test1")
	if err != nil {
		t.Fatalf("checkVMI: %v", err)
	}
	if state != vmiPending {
		t.Errorf("state = %v, want vmiPending (still within grace period), msg=%q", state, msg)
	}
}

func TestCheckVMI_SynchronizedFalseBeyondGracePeriod_Fails(t *testing.T) {
	vmi := vmiWithSyncFailure("workstation1", "vm-test1", "PVC pending", time.Now().Add(-5*time.Minute))
	cl := fake.NewClientBuilder().WithScheme(vmiScheme(t)).WithObjects(vmi).Build()
	v := &VMBooter{Client: cl}

	state, msg, err := v.checkVMI(context.Background(), "workstation1", "vm-test1")
	if err != nil {
		t.Fatalf("checkVMI: %v", err)
	}
	if state != vmiFailed {
		t.Errorf("state = %v, want vmiFailed (beyond grace period)", state)
	}
	if msg != "VMI Synchronized=False: PVC pending" {
		t.Errorf("msg = %q", msg)
	}
}

// TestHandleVM_OwnsEFIVarsPVCOnExistingVM confirms HandleVM patches the efivars PVC's
// ownerReference to the build VM as soon as the VM is found to already exist —
// covering the majority of a build's lifetime (Booting/Provisioning/CapturingDisks),
// not just the final TemplateProvisioning phase. Before this, a VM deleted anywhere in
// that window left its efivars PVC unowned and orphaned forever.
func TestHandleVM_OwnsEFIVarsPVCOnExistingVM(t *testing.T) {
	build := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "bld", Namespace: "vm-bld"},
		Status:     v1alpha1.VirtualMachineBuildStatus{BuildID: "bld"},
	}
	vmSpec := &v1alpha1.BuildVM{Name: "webserver", EFIFirmware: &v1alpha1.EFIFirmware{}}
	vmStatus := &v1alpha1.VMBuildStatus{}

	vmName := BuildNameForBuildVM(BuildID(build), vmSpec.Name)
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(vmGVK)
	vm.SetName(vmName)
	vm.SetNamespace("vm-bld")
	vm.SetUID(testVMUID)

	pvcName := efiPVCName(BuildID(build), vmSpec.Name)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvcName, Namespace: "vm-bld",
			// Pre-marked populated so IsEFIFirmwareReady short-circuits true without
			// needing a completed copy Job in this test's fixtures.
			Annotations: map[string]string{"aileron.ruddervirt.io/efi-populated": valueTrue},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(vmiScheme(t)).WithObjects(vm, pvc).Build()
	v := &VMBooter{Client: cl}

	if _, err := v.HandleVM(context.Background(), build, vmSpec, vmStatus); err != nil {
		t.Fatalf("HandleVM: %v", err)
	}

	got := &corev1.PersistentVolumeClaim{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: pvcName, Namespace: "vm-bld"}, got); err != nil {
		t.Fatal(err)
	}
	if len(got.OwnerReferences) != 1 || got.OwnerReferences[0].UID != testVMUID {
		t.Errorf("efivars PVC ownerReferences = %v, want owned by VM vm-uid-1", got.OwnerReferences)
	}
}

// TestHandleVM_EFIOwnershipFailureDoesNotFailVM confirms a transient failure patching
// the efivars PVC's ownership never fails the VM's phase — it's a resilience measure,
// not core booting logic, and turning it into a hard failure would make a build
// permanently fail over a missed metadata patch instead of retrying next reconcile.
func TestHandleVM_EFIOwnershipFailureDoesNotFailVM(t *testing.T) {
	build := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "bld", Namespace: "vm-bld"},
		Status:     v1alpha1.VirtualMachineBuildStatus{BuildID: "bld"},
	}
	vmSpec := &v1alpha1.BuildVM{Name: "webserver", EFIFirmware: &v1alpha1.EFIFirmware{}}
	vmStatus := &v1alpha1.VMBuildStatus{}

	vmName := BuildNameForBuildVM(BuildID(build), vmSpec.Name)
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(vmGVK)
	vm.SetName(vmName)
	vm.SetNamespace("vm-bld")
	vm.SetUID(testVMUID)

	pvcName := efiPVCName(BuildID(build), vmSpec.Name)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvcName, Namespace: "vm-bld",
			Annotations: map[string]string{"aileron.ruddervirt.io/efi-populated": valueTrue},
		},
	}

	base := fake.NewClientBuilder().WithScheme(vmiScheme(t)).WithObjects(vm, pvc).Build()
	cl := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			return errors.New("boom: transient API error")
		},
	})
	v := &VMBooter{Client: cl}

	phase, err := v.HandleVM(context.Background(), build, vmSpec, vmStatus)
	if err != nil {
		t.Fatalf("HandleVM should not fail on a transient efivars-ownership error, got: %v", err)
	}
	if phase == v1alpha1.VMPhaseFailed {
		t.Error("HandleVM returned VMPhaseFailed for a transient efivars-ownership error")
	}
}
