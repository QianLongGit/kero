package proto

// A transfer connection moves whole files between the host and a remote
// client, at a volume the control connection's JSON lines cannot carry.
// It opens with a Hello of KindTransfer, then one Request line says what
// runs: a download streams files out, an upload writes one the client
// sends. The bodies that follow are raw, each as long as its header
// declared, and the replies stay JSON, proto.Message, as a control
// connection's are.

// KindTransfer opens a transfer connection.
const KindTransfer = "transfer"

// The transfer methods, as a transfer connection's one Request line
// carries them.
const (
	// MethodTransferDownload streams the path the params name, and every
	// file under it, to the client.
	MethodTransferDownload = "transfer.download"
	// MethodTransferUpload writes a body the client sends, of Size bytes,
	// to Dir/Name, and answers where it landed.
	MethodTransferUpload = "transfer.upload"
)

// TransferDownloadParams name what a download streams out: a file, or a
// directory and all that is in it.
type TransferDownloadParams struct {
	Path string `json:"path"`
}

// TransferHeader precedes a body: for a file its Size and Mode, for a
// directory Dir with the Count of files under it and their total Size —
// the client shows progress against that. A zero Size stays in the JSON,
// an empty file is a body of its own.
type TransferHeader struct {
	Dir   bool   `json:"dir,omitempty"`
	Size  int64  `json:"size"`
	Count int    `json:"count,omitempty"`
	Mode  uint32 `json:"mode,omitempty"`
}

// TransferUploadParams say what an upload writes, the body that follows
// of Size bytes, with the mode to give the file.
type TransferUploadParams struct {
	Dir  string `json:"dir"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	Mode uint32 `json:"mode"`
}

// TransferUploadDone answers an upload with where the body landed.
type TransferUploadDone struct {
	Path string `json:"path"`
}
