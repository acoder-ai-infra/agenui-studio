package orchestration

import (
	"encoding/json"
	"testing"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/designing/contract"
)

func TestContentContractDeliveryModePreservesLegacyAndPreview(t *testing.T) {
	for _, test := range []struct {
		name string
		mode string
		want string
	}{
		{name: "legacy", want: contract.DeliveryModeExecutable},
		{name: "executable", mode: contract.DeliveryModeExecutable, want: contract.DeliveryModeExecutable},
		{name: "preview", mode: contract.DeliveryModeDesignPreview, want: contract.DeliveryModeDesignPreview},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(contract.Revision{
				SchemaVersion: contract.SchemaVersion,
				Draft:         contract.Draft{DeliveryMode: test.mode},
			})
			if err != nil {
				t.Fatal(err)
			}
			got, err := contentContractDeliveryMode(string(raw))
			if err != nil || got != test.want {
				t.Fatalf("mode=%q err=%v, want %q", got, err, test.want)
			}
		})
	}
}
