package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestValidateSpaceModel_RejectsEncryption(t *testing.T) {
	m := roomLikeModel{Encryption: types.BoolValue(true)}
	diags := validateSpaceModel(m)
	if !diags.HasError() {
		t.Fatalf("expected error for encryption_enabled on space, got %v", diags)
	}
}

func TestValidateSpaceModel_RejectsIsDirect(t *testing.T) {
	m := roomLikeModel{IsDirect: types.BoolValue(true)}
	diags := validateSpaceModel(m)
	if !diags.HasError() {
		t.Fatalf("expected error for is_direct on space, got %v", diags)
	}
}

func TestValidateSpaceModel_AllowsNeitherEncryptionNorIsDirect(t *testing.T) {
	m := roomLikeModel{
		Name:  types.StringValue("my-space"),
		Topic: types.StringValue("fine as a space"),
	}
	diags := validateSpaceModel(m)
	if diags.HasError() {
		t.Fatalf("unexpected error for valid space model: %v", diags)
	}
}

func TestValidateSpaceModel_RejectsBothTogether(t *testing.T) {
	m := roomLikeModel{
		Encryption: types.BoolValue(true),
		IsDirect:   types.BoolValue(true),
	}
	diags := validateSpaceModel(m)
	// Expect two separate errors, not one.
	if diags.ErrorsCount() != 2 {
		t.Fatalf("expected 2 errors, got %d (%v)", diags.ErrorsCount(), diags)
	}
}
