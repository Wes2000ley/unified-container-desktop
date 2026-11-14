package api

// TransferOpenParams identifies a streaming binary operation. Size is required
// for a container file upload so its tar header can precede the file contents.
type TransferOpenParams struct {
	Operation      string            `json:"operation"`
	ID             string            `json:"id,omitempty"`
	Path           string            `json:"path,omitempty"`
	Name           string            `json:"name,omitempty"`
	Reference      string            `json:"reference,omitempty"`
	References     []string          `json:"references,omitempty"`
	OperationID    string            `json:"operationId,omitempty"`
	Dockerfile     string            `json:"dockerfile,omitempty"`
	DockerfilePath string            `json:"dockerfilePath,omitempty"`
	Tags           []string          `json:"tags,omitempty"`
	BuildArgs      map[string]string `json:"buildArgs,omitempty"`
	Size           *int64            `json:"size,omitempty"`
	Mode           uint32            `json:"mode,omitempty"`
	Confirmed      bool              `json:"confirmed,omitempty"`
}

// TransferParams addresses a session and carries at most one 256 KiB chunk.
// JSON encodes Data as base64.
type TransferParams struct {
	SessionID string `json:"sessionID"`
	MaxBytes  int    `json:"maxBytes,omitempty"`
	Data      []byte `json:"data,omitempty"`
}

type TransferOpenResult struct {
	SessionID string `json:"sessionID"`
	Name      string `json:"name"`
	Size      *int64 `json:"size,omitempty"`
}

type TransferReadResult struct {
	Data []byte `json:"data"`
	EOF  bool   `json:"eof"`
}
