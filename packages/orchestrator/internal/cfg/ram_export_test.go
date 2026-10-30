package cfg

import "testing"

func TestRAMExportModeValidation(t *testing.T) {
	for _, mode := range []string{"", "full_logical", "tracked_delta", "mincore", "tracked"} {
		err := (BuilderConfig{RAMExportMode: mode}).ValidateRAMExportMode()
		valid := mode == "" || mode == "full_logical" || mode == "tracked_delta"
		if (err == nil) != valid {
			t.Fatalf("mode %q: %v", mode, err)
		}
	}
}
