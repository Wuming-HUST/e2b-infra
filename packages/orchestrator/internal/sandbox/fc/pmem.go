package fc

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/e2b-dev/infra/packages/shared/pkg/fc/models"
	"github.com/go-openapi/runtime"
	"github.com/go-openapi/strfmt"
)

// Use the same Unix transport, serializer and context as generated operations.
func (c *apiClient) setPmem(ctx context.Context, binding models.PmemConfig) error {
	if err := binding.ValidateBinding(); err != nil {
		return err
	}
	_, err := c.client.Transport.Submit(&runtime.ClientOperation{
		ID: "putPmem", Method: "PUT", PathPattern: "/pmem", ProducesMediaTypes: []string{"application/json"}, ConsumesMediaTypes: []string{"application/json"}, Schemes: []string{"http"}, Context: ctx,
		Params: runtime.ClientRequestWriterFunc(func(r runtime.ClientRequest, _ strfmt.Registry) error {
			if err := r.SetTimeout(30 * time.Second); err != nil {
				return err
			}
			return r.SetBodyParam(&binding)
		}),
		Reader: runtime.ClientResponseReaderFunc(func(r runtime.ClientResponse, _ runtime.Consumer) (interface{}, error) {
			if r.Code() == 204 {
				return nil, nil
			}
			body, _ := io.ReadAll(io.LimitReader(r.Body(), 8192))
			return nil, fmt.Errorf("pmem configuration rejected (%d): %s", r.Code(), body)
		}),
	})
	if err != nil {
		return fmt.Errorf("configure immutable pmem (requires compatible FC): %w", err)
	}
	return nil
}

type pmemBindingProvider interface {
	PmemBinding() models.PmemConfig
	PmemDataOffset() uint64
}

func isPmemProvider(p interface{}) bool { _, ok := p.(pmemBindingProvider); return ok }
