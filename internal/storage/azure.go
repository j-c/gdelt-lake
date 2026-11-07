package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
)

// Azure keeps the lake in one Azure Blob Storage container. Paths are blob names. A block blob upload is visible only
// once its block list is committed, which happens after the last block, so writes are atomic; a failed upload leaves
// only uncommitted blocks, which no reader sees and Azure discards after a week.
type Azure struct {
	c *container.Client
}

// AzureConfig says how to reach the container: either an account URL, with credentials from the standard Azure
// credential chain (managed identity in Azure, `az login` on a workstation), or a connection string (a SAS, or the
// Azurite emulator).
type AzureConfig struct {
	AccountURL       string // e.g. https://<account>.blob.core.windows.net/
	ConnectionString string
	Container        string
}

// NewAzure connects to the container, creating it if it doesn't exist.
func NewAzure(ctx context.Context, cfg AzureConfig) (*Azure, error) {
	var (
		svc *azblob.Client
		err error
	)
	switch {
	case cfg.ConnectionString != "":
		svc, err = azblob.NewClientFromConnectionString(cfg.ConnectionString, nil)
	case cfg.AccountURL != "":
		var cred azcore.TokenCredential
		if cred, err = azidentity.NewDefaultAzureCredential(nil); err == nil {
			svc, err = azblob.NewClient(cfg.AccountURL, cred, nil)
		}
	default:
		err = errors.New("an account URL or a connection string is required")
	}
	if err != nil {
		return nil, fmt.Errorf("azure blob storage: %w", err)
	}
	a := &Azure{c: svc.ServiceClient().NewContainerClient(cfg.Container)}
	if _, err := a.c.Create(ctx, nil); err != nil && !bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
		// Creating needs more rights than using; an identity limited to blob data may not have them. Carry on if the
		// container is there.
		if _, perr := a.c.GetProperties(ctx, nil); perr != nil {
			return nil, fmt.Errorf("azure blob storage: container %q: %w", cfg.Container, err)
		}
	}
	return a, nil
}

// NewAzureFromContainer wraps an existing container client (tests).
func NewAzureFromContainer(c *container.Client) *Azure { return &Azure{c: c} }

func (a *Azure) List(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	pager := a.c.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{Prefix: &prefix})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, b := range page.Segment.BlobItems {
			out = append(out, *b.Name)
		}
	}
	sort.Strings(out) // the service returns names sorted already; this keeps the contract explicit
	return out, nil
}

func (a *Azure) Exists(ctx context.Context, p string) (bool, error) {
	if err := CheckPath(p); err != nil {
		return false, err
	}
	_, err := a.c.NewBlobClient(p).GetProperties(ctx, nil)
	switch {
	case err == nil:
		return true, nil
	case notFound(err):
		return false, nil
	}
	return false, err
}

// blockSize is the size of each staged block: big enough for few round trips, small enough to keep memory modest
// (Write holds up to concurrency blocks in memory).
const (
	blockSize   = 8 << 20
	concurrency = 4
)

func (a *Azure) Write(ctx context.Context, p string, r io.Reader) error {
	if err := CheckPath(p); err != nil {
		return err
	}
	_, err := a.c.NewBlockBlobClient(p).UploadStream(ctx, r, &blockblob.UploadStreamOptions{
		BlockSize: blockSize, Concurrency: concurrency})
	return err
}

func (a *Azure) Open(ctx context.Context, p string) (io.ReadCloser, error) {
	if err := CheckPath(p); err != nil {
		return nil, err
	}
	resp, err := a.c.NewBlobClient(p).DownloadStream(ctx, nil)
	if err != nil {
		if notFound(err) {
			return nil, fmt.Errorf("%s: %w", p, ErrNotFound)
		}
		return nil, err
	}
	return resp.Body, nil
}

func (a *Azure) Delete(ctx context.Context, p string) error {
	if err := CheckPath(p); err != nil {
		return err
	}
	if _, err := a.c.NewBlobClient(p).Delete(ctx, nil); err != nil && !notFound(err) {
		return err
	}
	return nil
}

func notFound(err error) bool {
	var re *azcore.ResponseError
	return bloberror.HasCode(err, bloberror.BlobNotFound) || (errors.As(err, &re) && re.StatusCode == http.StatusNotFound)
}

// String names the backend in logs, without credentials.
func (a *Azure) String() string {
	u := a.c.URL()
	if i := strings.IndexByte(u, '?'); i >= 0 { // drop a SAS token
		u = u[:i]
	}
	return "azure " + u
}
