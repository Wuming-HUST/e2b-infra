package fc

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBootSourceOptionalInitrd(t *testing.T) {
	for _, initrd := range []string{"", "/fixtures/guest initrd.gz"} {
		t.Run(initrd, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fc.sock")
			listener, err := net.Listen("unix", path)
			require.NoError(t, err)
			bodies := make(chan map[string]any, 1)
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut || r.URL.Path != "/boot-source" {
					http.Error(w, "unexpected API", http.StatusNotFound)
					return
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				bodies <- body
				w.WriteHeader(http.StatusNoContent)
			})}
			go server.Serve(listener)
			defer server.Close()
			require.NoError(t, newApiClient(path).setBootSource(context.Background(), "init=/sbin/init", "/fixtures/vmlinuz", initrd))
			body := <-bodies
			require.Equal(t, "/fixtures/vmlinuz", body["kernel_image_path"])
			require.Equal(t, "init=/sbin/init", body["boot_args"])
			if initrd == "" {
				require.NotContains(t, body, "initrd_path")
			} else {
				require.Equal(t, initrd, body["initrd_path"])
			}
		})
	}
}
