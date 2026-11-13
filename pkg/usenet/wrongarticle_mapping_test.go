package usenet

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/nntp"
)

type errCursor struct{ err error }

func (c errCursor) ReadAtContext(context.Context, []byte, int64) (int, error) { return 0, c.err }
func (errCursor) Prefetch(context.Context, int64, int64)                       {}
func (errCursor) Close()                                                       {}

// An article whose every reachable copy is another post's is as permanent as
// a 430: the handle reports article-not-found (so the session reacquires) and
// later opens of the file are refused.
func TestFileHandleWrongArticleIsMissingArticle(t *testing.T) {
	wrong := fmt.Errorf("%w: %w", nntp.ErrAllProvidersFailed,
		nntp.NewWrongArticleError(errors.New("segment 41: provider returned part 139/208")))
	for name, cause := range map[string]error{
		"wrong-article": wrong,
		"not-found":     &nntp.Error{Type: nntp.ErrorTypeArticleNotFound, Message: "430"},
	} {
		t.Run(name, func(t *testing.T) {
			u := &Usenet{failedFiles: xsync.NewMap[string, error]()}
			h := &FileHandle{u: u, failureKey: "nzo/file.mkv", entry: &fsEntry{}, cursor: errCursor{cause}}
			_, err := h.ReadAtContext(context.Background(), make([]byte, 1), 0)
			if !customerror.IsArticleNotFoundError(err) {
				t.Fatalf("ReadAtContext error %v: want article-not-found", err)
			}
			stored, ok := u.failedFiles.Load("nzo/file.mkv")
			if !ok || !customerror.IsArticleNotFoundError(stored) {
				t.Fatalf("failedFiles = %v, %v: want article-not-found", stored, ok)
			}
		})
	}
	t.Run("transient-stays-transient", func(t *testing.T) {
		u := &Usenet{failedFiles: xsync.NewMap[string, error]()}
		h := &FileHandle{u: u, failureKey: "k", entry: &fsEntry{}, cursor: errCursor{&nntp.Error{Type: nntp.ErrorTypeTimeout}}}
		_, err := h.ReadAtContext(context.Background(), make([]byte, 1), 0)
		if customerror.IsArticleNotFoundError(err) {
			t.Fatalf("timeout mapped to article-not-found: %v", err)
		}
		if _, ok := u.failedFiles.Load("k"); ok {
			t.Fatal("timeout marked the file failed")
		}
	})
}
