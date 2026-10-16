package cli

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"
)

func TestStreamMasksTheSecret(t *testing.T) {
	const secret = "trk_abc"
	tests := []string{
		"",
		"no token here",
		"trk_abc",
		"before trk_abc after",
		"trk_abctrk_abc",
		"trk_trk_abc",
		"ends with a start: trk_ab",
		"xtrk_trk_abcy trk_",
	}
	for _, in := range tests {
		want := strings.ReplaceAll(in, secret, "[token]")
		// in as two writes split at every point, and a byte at a time.
		var ways [][]string
		for i := range len(in) + 1 {
			ways = append(ways, []string{in[:i], in[i:]})
		}
		ways = append(ways, strings.Split(in, ""))
		for _, writes := range ways {
			var log bytes.Buffer
			c := &capture{w: &log, h: sha256.New(), secret: []byte(secret)}
			s := &stream{c: c}
			for _, w := range writes {
				if n, err := s.Write([]byte(w)); n != len(w) || err != nil {
					t.Fatalf("Write(%q) = %d, %v", w, n, err)
				}
			}
			s.flush()
			sum := sha256.Sum256([]byte(want))
			if log.String() != want || c.n != int64(len(want)) || !bytes.Equal(c.h.Sum(nil), sum[:]) {
				t.Errorf("%q written as %q: kept %q (%d bytes), want %q", in, writes, log.String(), c.n, want)
			}
		}
	}
}

// Each stream holds back its own start of the secret, so output on the
// other stream between the parts does not hide it.
func TestStreamsInterleaved(t *testing.T) {
	var log bytes.Buffer
	c := &capture{w: &log, h: sha256.New(), secret: []byte("trk_abc")}
	stdout, stderr := &stream{c: c}, &stream{c: c}
	stdout.Write([]byte("token trk_"))
	stderr.Write([]byte("noise\n"))
	stdout.Write([]byte("abc\n"))
	stdout.flush()
	stderr.flush()
	if want := "token noise\n[token]\n"; log.String() != want {
		t.Errorf("kept %q, want %q", log.String(), want)
	}
}
