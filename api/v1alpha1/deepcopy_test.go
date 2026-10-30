package v1alpha1

import (
	"testing"
)

func TestIngressContractSpecDeepCopy(t *testing.T) {
	var nilSpec *IngressContractSpec
	if got := nilSpec.DeepCopy(); got != nil {
		t.Fatalf("DeepCopy of nil IngressContractSpec = %v, want nil", got)
	}

	in := &IngressContractSpec{Version: IngressContractVersionV1, SourceGeneration: 7}
	out := in.DeepCopy()
	if out == nil || out == in {
		t.Fatal("DeepCopy must return a new non-nil IngressContractSpec")
	}
	if *out != *in {
		t.Fatalf("DeepCopy = %+v, want %+v", *out, *in)
	}

	out.SourceGeneration = 9
	if in.SourceGeneration != 7 {
		t.Fatalf("DeepCopy must be independent: original mutated to %d", in.SourceGeneration)
	}
}

func TestWorkbenchesSpecDeepCopyIngressContract(t *testing.T) {
	withContract := &WorkbenchesSpec{
		IngressContract: &IngressContractSpec{Version: IngressContractVersionV1, SourceGeneration: 17},
	}
	out := withContract.DeepCopy()
	if out.IngressContract == nil || *out.IngressContract != *withContract.IngressContract {
		t.Fatalf("DeepCopy lost ingressContract: %+v", out.IngressContract)
	}
	out.IngressContract.Version = "mutated"
	if withContract.IngressContract.Version != IngressContractVersionV1 {
		t.Fatalf("DeepCopy must deep-copy ingressContract: original mutated to %q", withContract.IngressContract.Version)
	}

	withoutContract := &WorkbenchesSpec{}
	if got := withoutContract.DeepCopy().IngressContract; got != nil {
		t.Fatalf("DeepCopy of nil ingressContract = %v, want nil", got)
	}
}

func TestWorkbenchesDeepCopyIngressContract(t *testing.T) {
	wb := &Workbenches{}
	wb.Spec.IngressContract = &IngressContractSpec{Version: IngressContractVersionV1, SourceGeneration: 3}
	out := wb.DeepCopy()
	if out.Spec.IngressContract == nil || *out.Spec.IngressContract != *wb.Spec.IngressContract {
		t.Fatalf("Workbenches.DeepCopy lost ingressContract: %+v", out.Spec.IngressContract)
	}
}
