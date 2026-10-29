package proto

import (
	"encoding/json"
	"testing"
)

// A transfer's wire form is its contract: a struct that marshals
// differently breaks against a daemon of another build.
func TestTransferJSON(t *testing.T) {
	for _, c := range []struct {
		name string
		v    any
		want string
	}{
		{"download params", TransferDownloadParams{Path: "/src/kero"}, `{"path":"/src/kero"}`},
		{"header", TransferHeader{Size: 5}, `{"size":5}`},
		{"header zero", TransferHeader{}, `{"size":0}`},
		{"header full", TransferHeader{Dir: true, Size: 5, Count: 3, Mode: 0o644}, `{"dir":true,"size":5,"count":3,"mode":420}`},
		{"upload params", TransferUploadParams{Dir: "/src", Name: "kero.tar", Size: 42, Mode: 0o644}, `{"dir":"/src","name":"kero.tar","size":42,"mode":420}`},
		{"upload done", TransferUploadDone{Path: "/src/kero.tar"}, `{"path":"/src/kero.tar"}`},
	} {
		got, err := json.Marshal(c.v)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if string(got) != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func TestTransferKinds(t *testing.T) {
	for got, want := range map[string]string{
		KindTransfer:           "transfer",
		MethodTransferDownload: "transfer.download",
		MethodTransferUpload:   "transfer.upload",
	} {
		if got != want {
			t.Errorf("constant %q, want %q", got, want)
		}
	}
}
