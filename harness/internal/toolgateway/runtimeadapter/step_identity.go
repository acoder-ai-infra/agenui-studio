package runtimeadapter

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"

	"github.com/AGenUI/agenui-studio/harness/internal/identifiercontract"
)

// composeToolStepID keeps the durable Step identity independent from an
// opaque provider ToolCallID while remaining stable across retry and Resume.
func composeToolStepID(runID, toolCallID string) string {
	hash := sha256.New()
	var length [8]byte
	for _, part := range []string{runID, toolCallID} {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(part))
	}
	return "tool_" + base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
}

func validateResolvedStepIdentity(stepID, parentStepID string) error {
	if err := identifiercontract.Validate(
		identifiercontract.StepID(stepID),
		identifiercontract.ParentStepID(parentStepID),
	); err != nil {
		return errors.Join(ErrToolStepIdentityInvalid, err)
	}
	return nil
}
