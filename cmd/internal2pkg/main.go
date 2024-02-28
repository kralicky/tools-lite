package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kralicky/tools-lite/gopls/pkg/lsp/protocol"
	"github.com/kralicky/tools-lite/pkg/jsonrpc2"
)

// usage: internal2pkg "./path/to/internal"
func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: internal2pkg ./path/to/internal\n")
		return
	}

	if d, err := os.Stat(os.Args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "failed to stat %s: %v\n", os.Args[1], err)
		return
	} else if !d.IsDir() || path.Base(os.Args[1]) != "internal" {
		fmt.Fprintf(os.Stderr, "%s is not a directory named internal\n", os.Args[1])
		return
	}

	// create internal2pkg.go
	internal2pkgPath := path.Join(os.Args[1], "internal2pkg.go")
	if err := os.WriteFile(internal2pkgPath, []byte("package internal"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create internal2pkg.go: %v\n", err)
		return
	}
	defer os.Remove(path.Join(path.Dir(os.Args[1]), "pkg/internal2pkg.go"))

	// spawn a new gopls instance
	tempDir, err := os.MkdirTemp("", "internal2pkg")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create temp dir: %v\n", err)
		return
	}
	defer os.RemoveAll(tempDir)
	socketPath := path.Join(tempDir, "gopls.sock")
	cmd := exec.Command("gopls", "serve", "-rpc.trace", "-listen", "unix;"+socketPath)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)

		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		err := cmd.Run()
		if err != nil {
			fmt.Fprintf(os.Stderr, "gopls exited with error: %v\n", err)
		}
	}()

	// wait for the socket to be created
	for {
		select {
		case <-done:
			return
		default:
		}

		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// dial the socket
	netConn, err := net.Dial("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to dial the socket: %v\n", err)
		return
	}
	stream := jsonrpc2.NewHeaderStream(netConn)
	cc := jsonrpc2.NewConn(stream)
	dispatch := protocol.ServerDispatcher(cc)
	client := &cmdClient{
		files: map[protocol.DocumentURI]*cmdFile{},
	}
	cc.Go(context.TODO(),
		protocol.Handlers(
			protocol.ClientHandler(client, jsonrpc2.MethodNotFound)))

	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error finding workdir: %v\n", err)
		return
	}
	params := &protocol.ParamInitialize{}
	params.WorkspaceFolders = []protocol.WorkspaceFolder{
		{
			URI:  "file://" + wd,
			Name: "internal2pkg",
		},
	}
	params.Capabilities.Workspace.Configuration = true
	// If you add an additional option here, you must update the map key in connect.
	params.Capabilities.TextDocument.Hover = &protocol.HoverClientCapabilities{
		ContentFormat: []protocol.MarkupKind{protocol.Markdown},
	}
	params.Capabilities.TextDocument.SemanticTokens = protocol.SemanticTokensClientCapabilities{}
	params.Capabilities.TextDocument.SemanticTokens.Formats = []protocol.TokenFormat{"relative"}
	params.Capabilities.TextDocument.SemanticTokens.Requests.Range = &protocol.Or_ClientSemanticTokensRequestOptions_range{Value: true}
	// params.Capabilities.TextDocument.SemanticTokens.Requests.Range.Value = true
	params.Capabilities.TextDocument.SemanticTokens.Requests.Full = &protocol.Or_ClientSemanticTokensRequestOptions_full{Value: true}
	params.Capabilities.TextDocument.SemanticTokens.TokenTypes = protocol.SemanticTypes()
	params.Capabilities.TextDocument.SemanticTokens.TokenModifiers = protocol.SemanticModifiers()

	params.InitializationOptions = map[string]interface{}{
		"symbolMatcher": string("Fuzzy"),
	}
	params.RootURI = protocol.URIFromPath(wd)
	params.Capabilities.Workspace.WorkspaceEdit = &protocol.WorkspaceEditClientCapabilities{
		DocumentChanges: true,
		ResourceOperations: []protocol.ResourceOperationKind{
			protocol.Create,
			protocol.Rename,
			protocol.Delete,
		},
	}

	_, err = dispatch.Initialize(context.Background(), params)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize: %v\n", err)
		return
	}
	err = dispatch.Initialized(context.Background(), &protocol.InitializedParams{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to call initialized: %v\n", err)
		return
	}

	err = dispatch.DidOpen(context.Background(), &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:        protocol.URIFromPath(internal2pkgPath),
			LanguageID: "go",
			Version:    1,
			Text:       "package internal",
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to call textdocument/didOpen: %v\n", err)
		return
	}

	renameParams := &protocol.RenameParams{
		TextDocument: protocol.TextDocumentIdentifier{
			URI: protocol.URIFromPath(internal2pkgPath),
		},
		Position: protocol.Position{
			Line:      0,
			Character: 15,
		},
		NewName: "pkg",
	}
	_, err = dispatch.PrepareRename(context.Background(), &protocol.PrepareRenameParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: renameParams.TextDocument,
			Position:     renameParams.Position,
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to call textDocument/prepareRename: %v\n", err)
		return
	}

	edits, err := dispatch.Rename(context.Background(), renameParams)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to call textDocument/rename: %v\n", err)
		return
	}
	if err := client.applyWorkspaceEdit(edits); err != nil {
		fmt.Fprintf(os.Stderr, "failed to apply workspace edits: %v\n", err)
		return
	}
	fmt.Printf("applied %d edits\n", len(edits.DocumentChanges))
}

type cmdClient struct {
	filesMu sync.Mutex // guards files map and each cmdFile.diagnostics
	files   map[protocol.DocumentURI]*cmdFile
}
type cmdFile struct {
	uri         protocol.DocumentURI
	mapper      *protocol.Mapper
	err         error
	diagnostics []protocol.Diagnostic
}

// ApplyEdit implements protocol.Client.
func (c *cmdClient) ApplyEdit(context.Context, *protocol.ApplyWorkspaceEditParams) (*protocol.ApplyWorkspaceEditResult, error) {
	return nil, nil
}

func (cli *cmdClient) applyWorkspaceEdit(edit *protocol.WorkspaceEdit) error {
	var orderedURIs []protocol.DocumentURI
	edits := map[protocol.DocumentURI][]protocol.TextEdit{}
	renames := []protocol.RenameFile{}
	for _, c := range edit.DocumentChanges {
		if c.TextDocumentEdit != nil {
			uri := c.TextDocumentEdit.TextDocument.URI
			edits[uri] = append(edits[uri], c.TextDocumentEdit.Edits...)
			orderedURIs = append(orderedURIs, uri)
		}
		if c.RenameFile != nil {
			renames = append(renames, *c.RenameFile)
		}
	}
	slices.Sort(orderedURIs)
	for _, uri := range orderedURIs {
		f := cli.openFile(uri)
		if f.err != nil {
			return f.err
		}
		if err := applyTextEdits(f.mapper, edits[uri]); err != nil {
			return err
		}
	}
	for _, r := range renames {
		fmt.Printf("rename: %v -> %v\n", r.OldURI, r.NewURI)
		if err := os.Rename(r.OldURI.Path(), r.NewURI.Path()); err != nil {
			return err
		}
	}
	return nil
}

func (c *cmdClient) openFile(uri protocol.DocumentURI) *cmdFile {
	c.filesMu.Lock()
	defer c.filesMu.Unlock()
	return c.getFile(uri)
}

func (c *cmdClient) getFile(uri protocol.DocumentURI) *cmdFile {
	file, found := c.files[uri]
	if !found || file.err != nil {
		file = &cmdFile{
			uri: uri,
		}
		c.files[uri] = file
	}
	if file.mapper == nil {
		content, err := os.ReadFile(uri.Path())
		if err != nil {
			file.err = fmt.Errorf("getFile: %v: %v", uri, err)
			return file
		}
		file.mapper = protocol.NewMapper(uri, content)
	}
	return file
}

// applyTextEdits applies a list of edits to the mapper file content,
// using the preferred edit mode. It is a no-op if there are no edits.
func applyTextEdits(mapper *protocol.Mapper, edits []protocol.TextEdit) error {
	if len(edits) == 0 {
		return nil
	}
	newContent, _, err := protocol.ApplyEdits(mapper, edits)
	if err != nil {
		return err
	}

	filename := mapper.URI.Path()

	fmt.Println(filename)

	if err := os.WriteFile(filename, newContent, 0o644); err != nil {
		return err
	}

	return nil
}

// CodeLensRefresh implements protocol.Client.
func (c *cmdClient) CodeLensRefresh(context.Context) error {
	return nil
}

// Configuration implements protocol.Client.
func (c *cmdClient) Configuration(_ context.Context, params *protocol.ParamConfiguration) ([]interface{}, error) {
	results := make([]interface{}, len(params.Items))
	for i, item := range params.Items {
		if item.Section != "gopls" {
			continue
		}
		env := map[string]interface{}{}
		for _, value := range os.Environ() {
			l := strings.SplitN(value, "=", 2)
			if len(l) != 2 {
				continue
			}
			env[l[0]] = l[1]
		}
		m := map[string]interface{}{
			"env": env,
			"analyses": map[string]bool{
				"fillreturns":    true,
				"nonewvars":      true,
				"noresultvalues": true,
				"undeclaredname": true,
			},
			"verboseOutput": true,
		}
		results[i] = m
	}
	return results, nil
}

// DiagnosticRefresh implements protocol.Client.
func (c *cmdClient) DiagnosticRefresh(context.Context) error {
	return nil
}

// Event implements protocol.Client.
func (c *cmdClient) Event(context.Context, *interface{}) error {
	return nil
}

// FoldingRangeRefresh implements protocol.Client.
func (c *cmdClient) FoldingRangeRefresh(context.Context) error {
	return nil
}

// InlayHintRefresh implements protocol.Client.
func (c *cmdClient) InlayHintRefresh(context.Context) error {
	return nil
}

// InlineValueRefresh implements protocol.Client.
func (c *cmdClient) InlineValueRefresh(context.Context) error {
	return nil
}

// LogMessage implements protocol.Client.
func (c *cmdClient) LogMessage(ctx context.Context, params *protocol.LogMessageParams) error {
	switch params.Type {
	case protocol.Error:
		fmt.Fprintln(os.Stderr, "Error:", params.Message)
	case protocol.Warning:
		fmt.Fprintln(os.Stderr, "Warning:", params.Message)
	}
	return nil
}

// LogTrace implements protocol.Client.
func (c *cmdClient) LogTrace(context.Context, *protocol.LogTraceParams) error {
	return nil
}

// Progress implements protocol.Client.
func (c *cmdClient) Progress(context.Context, *protocol.ProgressParams) error {
	return nil
}

// PublishDiagnostics implements protocol.Client.
func (c *cmdClient) PublishDiagnostics(_ context.Context, params *protocol.PublishDiagnosticsParams) error {
	fmt.Fprintf(os.Stderr, "diagnostics for %s:\n", params.URI)
	for _, d := range params.Diagnostics {
		fmt.Fprintf(os.Stderr, "%s\n", d.Message)
	}
	return nil
}

// RegisterCapability implements protocol.Client.
func (c *cmdClient) RegisterCapability(context.Context, *protocol.RegistrationParams) error {
	return nil
}

// SemanticTokensRefresh implements protocol.Client.
func (c *cmdClient) SemanticTokensRefresh(context.Context) error {
	return nil
}

// ShowDocument implements protocol.Client.
func (c *cmdClient) ShowDocument(context.Context, *protocol.ShowDocumentParams) (*protocol.ShowDocumentResult, error) {
	return nil, nil
}

// ShowMessage implements protocol.Client.
func (c *cmdClient) ShowMessage(ctx context.Context, params *protocol.ShowMessageParams) error {
	return nil
}

// ShowMessageRequest implements protocol.Client.
func (c *cmdClient) ShowMessageRequest(context.Context, *protocol.ShowMessageRequestParams) (*protocol.MessageActionItem, error) {
	return nil, nil
}

// UnregisterCapability implements protocol.Client.
func (c *cmdClient) UnregisterCapability(context.Context, *protocol.UnregistrationParams) error {
	return nil
}

// WorkDoneProgressCreate implements protocol.Client.
func (c *cmdClient) WorkDoneProgressCreate(context.Context, *protocol.WorkDoneProgressCreateParams) error {
	return nil
}

// WorkspaceFolders implements protocol.Client.
func (c *cmdClient) WorkspaceFolders(context.Context) ([]protocol.WorkspaceFolder, error) {
	return nil, nil
}

var _ protocol.Client = (*cmdClient)(nil)
