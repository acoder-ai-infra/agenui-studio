package identifiercontract

import (
	"strings"
	"testing"
)

func TestValidateCharacterBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		field   Field
		wantErr bool
	}{
		{name: "tenant 64 ASCII", field: TenantID(strings.Repeat("t", MaxTenantIDCharacters))},
		{name: "tenant 64 UTF-8", field: TenantID(strings.Repeat("租", MaxTenantIDCharacters))},
		{name: "tenant 65", field: TenantID(strings.Repeat("t", MaxTenantIDCharacters+1)), wantErr: true},
		{name: "agent 128 ASCII", field: AgentID(strings.Repeat("a", MaxAgentIDCharacters))},
		{name: "agent 128 UTF-8", field: AgentID(strings.Repeat("代", MaxAgentIDCharacters))},
		{name: "agent 129", field: AgentID(strings.Repeat("a", MaxAgentIDCharacters+1)), wantErr: true},
		{name: "idempotency 128", field: IdempotencyKey(strings.Repeat("i", MaxIdempotencyKeyCharacters))},
		{name: "idempotency 129", field: IdempotencyKey(strings.Repeat("i", MaxIdempotencyKeyCharacters+1)), wantErr: true},
		{name: "composite idempotency 320", field: GenericIdempotencyID(strings.Repeat("i", MaxGenericIdempotencyIDCharacters))},
		{name: "composite idempotency 321", field: GenericIdempotencyID(strings.Repeat("i", MaxGenericIdempotencyIDCharacters+1)), wantErr: true},
		{name: "empty optional field", field: SessionID("")},
		{name: "invalid UTF-8", field: UserID(string([]byte{0xff})), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.field)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidationErrorDoesNotExposeIdentifier(t *testing.T) {
	value := strings.Repeat("secret-user-id", 10)
	err := Validate(UserID(value))
	if err == nil {
		t.Fatal("Validate() error = nil")
	}
	if strings.Contains(err.Error(), value) {
		t.Fatalf("Validate() exposed identifier value: %v", err)
	}
}

func TestComposeIdempotencyKeyPreservesShortValues(t *testing.T) {
	if got := ComposeIdempotencyKey("run-1", "tool-1", "started"); got != "run-1:tool-1:started" {
		t.Fatalf("ComposeIdempotencyKey() = %q", got)
	}
}

func TestComposeIdempotencyKeyBoundsLongAndInvalidValues(t *testing.T) {
	longPart := strings.Repeat("幂", MaxIdempotencyKeyCharacters)
	first := ComposeIdempotencyKey("agent-chat-artifact", longPart)
	second := ComposeIdempotencyKey("agent-chat-artifact", longPart)
	if first != second {
		t.Fatalf("ComposeIdempotencyKey() is unstable: %q != %q", first, second)
	}
	if err := Validate(IdempotencyKey(first)); err != nil {
		t.Fatalf("composed key is invalid: %v", err)
	}
	if first == ComposeIdempotencyKey("agent-chat-artifact", longPart+"x") {
		t.Fatal("different inputs produced the same composed key")
	}
	if err := Validate(IdempotencyKey(ComposeIdempotencyKey("invalid", string([]byte{0xff})))); err != nil {
		t.Fatalf("invalid UTF-8 input did not produce a valid key: %v", err)
	}
}

func TestComposeIdempotencyKeySeparatesPartBoundaries(t *testing.T) {
	left := ComposeIdempotencyKey(strings.Repeat("a", MaxIdempotencyKeyCharacters), "b:c")
	right := ComposeIdempotencyKey(strings.Repeat("a", MaxIdempotencyKeyCharacters), "b", "c")
	if left == right {
		t.Fatal("different part boundaries produced the same composed key")
	}
}
