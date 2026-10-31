package fc

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/template"
	"github.com/e2b-dev/infra/packages/shared/pkg/fc/models"
)

func TestPmemAPIAndSnapshotBindingUseUnixTransport(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "fc.sock")
	listener, e := net.Listen("unix", socket)
	if e != nil {
		t.Fatal(e)
	}
	requests := make(chan map[string]json.RawMessage, 4)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			t.Error(e)
		}
		requests <- body
		if r.URL.Path == "/pmem" && r.Method != "PUT" {
			t.Error("wrong method")
		}
		w.WriteHeader(204)
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close(); os.Remove(socket) })
	binding := models.PmemConfig{PmemID: "rootfs-lower", PathOnHost: "/cache/lower.raw", ContentID: strings.Repeat("a", 64), GuestAddress: 1 << 38, Size: 128 << 20, ReadOnly: true}
	client := newApiClient(socket)
	if e = client.setPmem(t.Context(), binding); e != nil {
		t.Fatal(e)
	}
	body := <-requests
	var got models.PmemConfig
	data, _ := json.Marshal(body)
	if e = json.Unmarshal(data, &got); e != nil {
		t.Fatal(e)
	}
	if got != binding {
		t.Fatalf("%+v", got)
	}
	backend := NewFileSnapshotMemoryBackend("/memory")
	backend.Pmem = &binding
	if e = client.loadSnapshot(context.Background(), backend, template.NewLocalFileLink("/state")); e != nil {
		t.Fatal(e)
	}
	body = <-requests
	if e = json.Unmarshal(body["pmem_override"], &got); e != nil {
		t.Fatal(e)
	}
	if got != binding {
		t.Fatal("restore lost binding")
	}
	binding.ReadOnly = false
	if e = client.setPmem(t.Context(), binding); e == nil {
		t.Fatal("accepted writable binding")
	}
}
