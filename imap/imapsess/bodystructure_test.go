package imapsess

import (
	"errors"
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestDecodeBodyStructureEmpty(t *testing.T) {
	if _, err := decodeBodyStructure(nil); !errors.Is(err, errEmptyBodyStructure) {
		t.Errorf("nil input: err = %v, want errEmptyBodyStructure", err)
	}
	if _, err := decodeBodyStructure([]byte(`{}`)); !errors.Is(err, errEmptyBodyStructure) {
		t.Errorf(`"{}" input: err = %v, want errEmptyBodyStructure`, err)
	}
}

func TestDecodeBodyStructureSinglePartText(t *testing.T) {
	raw := []byte(`{
		"type":"text","subtype":"plain",
		"params":{"charset":"utf-8"},
		"encoding":"7bit","size":42,"lines":3
	}`)
	bs, err := decodeBodyStructure(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	sp, ok := bs.(*imap.BodyStructureSinglePart)
	if !ok {
		t.Fatalf("got %T, want *BodyStructureSinglePart", bs)
	}
	if sp.Type != "text" || sp.Subtype != "plain" {
		t.Errorf("media type %s/%s, want text/plain", sp.Type, sp.Subtype)
	}
	if sp.Params["charset"] != "utf-8" {
		t.Errorf("charset = %q, want utf-8", sp.Params["charset"])
	}
	if sp.Encoding != "7bit" {
		t.Errorf("encoding = %q, want 7bit", sp.Encoding)
	}
	if sp.Size != 42 {
		t.Errorf("size = %d, want 42", sp.Size)
	}
	if sp.Text == nil || sp.Text.NumLines != 3 {
		t.Errorf("Text = %+v, want NumLines=3", sp.Text)
	}
}

func TestDecodeBodyStructureMultipart(t *testing.T) {
	raw := []byte(`{
		"type":"multipart","subtype":"alternative",
		"params":{"boundary":"--foo"},
		"size":0,
		"parts":[
			{"type":"text","subtype":"plain","size":10},
			{"type":"text","subtype":"html","size":20,"lines":1}
		]
	}`)
	bs, err := decodeBodyStructure(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	mp, ok := bs.(*imap.BodyStructureMultiPart)
	if !ok {
		t.Fatalf("got %T, want *BodyStructureMultiPart", bs)
	}
	if mp.Subtype != "alternative" {
		t.Errorf("subtype = %q, want alternative", mp.Subtype)
	}
	if len(mp.Children) != 2 {
		t.Fatalf("len(Children) = %d, want 2", len(mp.Children))
	}
	if mp.Extended == nil || mp.Extended.Params["boundary"] != "--foo" {
		t.Errorf("Extended.Params = %+v, want boundary=--foo", mp.Extended)
	}

	plain, ok := mp.Children[0].(*imap.BodyStructureSinglePart)
	if !ok || plain.Subtype != "plain" {
		t.Errorf("Children[0] = %+v, want text/plain", mp.Children[0])
	}
	html, ok := mp.Children[1].(*imap.BodyStructureSinglePart)
	if !ok || html.Subtype != "html" || html.Text == nil || html.Text.NumLines != 1 {
		t.Errorf("Children[1] = %+v, want text/html with 1 line", mp.Children[1])
	}
}

func TestDecodeBodyStructureDisposition(t *testing.T) {
	raw := []byte(`{
		"type":"application","subtype":"pdf",
		"size":99,
		"disposition":{"type":"attachment","params":{"filename":"x.pdf"}}
	}`)
	bs, err := decodeBodyStructure(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	disp := bs.Disposition()
	if disp == nil {
		t.Fatal("Disposition() = nil, want non-nil")
	}
	if disp.Value != "attachment" {
		t.Errorf("Value = %q, want attachment", disp.Value)
	}
	if disp.Params["filename"] != "x.pdf" {
		t.Errorf("Params = %+v, want filename=x.pdf", disp.Params)
	}
}
