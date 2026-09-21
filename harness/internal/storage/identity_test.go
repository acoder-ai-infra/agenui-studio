package storage

import (
	"strings"
	"testing"
)

func TestValidateUserIDCharacterBoundary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		userID  string
		wantErr bool
	}{
		{name: "empty"},
		{name: "64 ASCII characters", userID: strings.Repeat("u", MaxUserIDCharacters)},
		{name: "64 UTF-8 characters", userID: strings.Repeat("用", MaxUserIDCharacters)},
		{name: "65 ASCII characters", userID: strings.Repeat("u", MaxUserIDCharacters+1), wantErr: true},
		{name: "65 UTF-8 characters", userID: strings.Repeat("用", MaxUserIDCharacters+1), wantErr: true},
		{name: "invalid UTF-8", userID: string([]byte{0xff}), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateUserID(tc.userID)
			if tc.wantErr != IsErrorCode(err, ErrInvalidArgument) {
				t.Fatalf("ValidateUserID() error = %v, want ErrInvalidArgument=%v", err, tc.wantErr)
			}
		})
	}
}
