package api

import (
	"archive/tar"
	"compress/gzip"
	"crypto/hmac"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	v1 "github.com/garethgeorge/backrest/gen/go/v1"
	"github.com/garethgeorge/backrest/internal/oplog"
	"go.uber.org/zap"
)

func NewDownloadHandler(oplog *oplog.OpLog) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path[1:]

		opID, signature, filePath, err := parseDownloadPath(p)
		if err != nil {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}

		if ok, err := checkDownloadURLSignature(opID, signature); err != nil || !ok {
			http.Error(w, fmt.Sprintf("invalid signature: %v", err), http.StatusForbidden)
			return
		}

		op, err := oplog.Get(int64(opID))
		if err != nil {
			http.Error(w, "restore not found", http.StatusNotFound)
			return
		}
		restoreOp, ok := op.Op.(*v1.Operation_OperationRestore)
		if !ok {
			http.Error(w, "restore not found", http.StatusNotFound)
			return
		}
		targetPath := restoreOp.OperationRestore.GetTarget()
		if targetPath == "" {
			http.Error(w, "restore target not found", http.StatusNotFound)
			return
		}
		fullPath := filepath.Join(targetPath, filePath)

		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=archive-%v.tar.gz", time.Now().Format("2006-01-02-15-04-05")))
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Transfer-Encoding", "binary")

		gzw := gzip.NewWriter(w)
		defer gzw.Close()
		t := tar.NewWriter(gzw)
		zap.L().Info("creating tar archive", zap.String("path", fullPath))
		if err := filepath.Walk(fullPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}

			stat, err := os.Stat(path)
			if err != nil {
				zap.L().Warn("error stating file", zap.String("path", path), zap.Error(err))
				return nil
			}
			file, err := os.OpenFile(path, os.O_RDONLY, 0)
			if err != nil {
				zap.L().Warn("error opening file", zap.String("path", path), zap.Error(err))
				return nil
			}
			defer file.Close()

			if err := t.WriteHeader(&tar.Header{
				Name:    path[len(fullPath)+1:],
				Size:    stat.Size(),
				Mode:    int64(stat.Mode()),
				ModTime: stat.ModTime(),
			}); err != nil {
				zap.L().Warn("error writing tar header", zap.String("path", path), zap.Error(err))
				return nil
			}
			if n, err := io.CopyN(t, file, stat.Size()); err != nil {
				zap.L().Warn("error copying file to tar archive", zap.String("path", path), zap.Error(err))
			} else if n != stat.Size() {
				zap.L().Warn("error copying file to tar archive: short write", zap.String("path", path))
			}
			return nil
		}); err != nil {
			zap.S().Errorf("error creating tar archive: %v", err)
			http.Error(w, "error creating tar archive", http.StatusInternalServerError)
		}
		t.Flush()
		if err := t.Close(); err != nil {
			zap.S().Errorf("error closing tar archive: %v", err)
			http.Error(w, "error closing tar archive", http.StatusInternalServerError)
		}
	})
}

func parseDownloadPath(p string) (int64, string, string, error) {
	sep := strings.Index(p, "/")
	if sep == -1 {
		return 0, "", "", fmt.Errorf("invalid path")
	}
	restoreID := p[:sep]
	filePath := p[sep+1:]

	dash := strings.Index(restoreID, "-")
	if dash == -1 {
		return 0, "", "", fmt.Errorf("invalid restore ID")
	}
	opID, err := strconv.ParseInt(restoreID[:dash], 16, 64)
	if err != nil {
		return 0, "", "", fmt.Errorf("invalid restore ID: %w", err)
	}
	signature := restoreID[dash+1:]
	return opID, signature, filePath, nil
}

func checkDownloadURLSignature(id int64, signature string) (bool, error) {
	wantSignatureBytes, err := signOperationIDForDownload(id)
	if err != nil {
		return false, err
	}
	signatureBytes, err := hex.DecodeString(signature)
	if err != nil {
		return false, err
	}
	return hmac.Equal(wantSignatureBytes, signatureBytes), nil
}

func signOperationIDForDownload(id int64) ([]byte, error) {
	opIDBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(opIDBytes, uint64(id))
	signature, err := generateSignature(opIDBytes)
	if err != nil {
		return nil, err
	}
	return signature, nil
}
