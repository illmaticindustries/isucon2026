package isureal

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"runtime"
	"sync"
	"time"
)

// Image signatures are pure CPU work (1000-round HMAC) while asia1, which serves
// every region, is CPU-bound and europe1/america1 are idle. Handlers that sign
// many images at once (Memories, export) send the uncached ones to the idle
// hosts over the private network in one round trip and store the results in the
// local cache; any failure falls back to signing locally on demand.

const (
	remoteSignMinBatch = 16
	remoteSignTimeout  = 500 * time.Millisecond
	remoteSignMaxItems = 1 << 16
)

var remoteSigners []string

var remoteSignClient = &http.Client{Transport: &http.Transport{
	MaxIdleConnsPerHost: 32,
	IdleConnTimeout:     5 * time.Minute,
}}

// SetRemoteSigners configures the signature services; call before serving.
func SetRemoteSigners(addresses []string) {
	remoteSigners = addresses
}

type signatureRequest struct {
	key     []byte
	imageID string
}

func appendImageSignatureRequests(requests []signatureRequest, user User, imageID string) []signatureRequest {
	if len(user.ImageKey) == sha256.Size && imageID != "" && len(imageID) <= 255 {
		requests = append(requests, signatureRequest{key: user.ImageKey, imageID: imageID})
	}
	return requests
}

func appendProfileSignatureRequest(requests []signatureRequest, user User) []signatureRequest {
	if user.ProfileImageID == nil {
		return requests
	}
	return appendImageSignatureRequests(requests, user, *user.ProfileImageID)
}

// prefetchImageSignatures fills the signature cache for the current expiry
// bucket. It never fails: whatever is not fetched is signed locally later.
func prefetchImageSignatures(ctx context.Context, requests []signatureRequest) {
	if len(remoteSigners) == 0 || len(requests) < remoteSignMinBatch {
		return
	}
	exp := imageSignatureExpiry(time.Now())
	seen := make(map[signatureCacheKey]bool, len(requests))
	missing := make([]signatureRequest, 0, len(requests))
	for _, request := range requests {
		cacheKey := signatureCacheKey{key: string(request.key), imageID: request.imageID, exp: exp}
		if seen[cacheKey] {
			continue
		}
		seen[cacheKey] = true
		if _, ok := loadImageSignature(cacheKey); ok {
			continue
		}
		missing = append(missing, request)
	}
	if len(missing) < remoteSignMinBatch || len(missing) > remoteSignMaxItems {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, remoteSignTimeout)
	defer cancel()
	shards := min(len(remoteSigners), (len(missing)+remoteSignMinBatch-1)/remoteSignMinBatch)
	var wg sync.WaitGroup
	for i := range shards {
		part := missing[len(missing)*i/shards : len(missing)*(i+1)/shards]
		wg.Add(1)
		go func(address string) {
			defer wg.Done()
			signatures, err := requestRemoteSignatures(ctx, address, exp, part)
			if err != nil {
				log.Printf("remote signer %s: %v", address, err)
				return
			}
			for j, request := range part {
				storeImageSignature(signatureCacheKey{key: string(request.key), imageID: request.imageID, exp: exp}, signatures[j])
			}
		}(remoteSigners[i])
	}
	wg.Wait()
}

// Wire format: request = exp(int64) count(uint32) then per item key[32]
// idLen(uint8) id; response = count signatures of 32 bytes in request order.

func encodeSignatureRequest(exp int64, requests []signatureRequest) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, exp)
	_ = binary.Write(&b, binary.BigEndian, uint32(len(requests)))
	for _, request := range requests {
		b.Write(request.key)
		b.WriteByte(byte(len(request.imageID)))
		b.WriteString(request.imageID)
	}
	return b.Bytes()
}

func decodeSignatureRequest(r io.Reader) (int64, []signatureRequest, error) {
	br := bufio.NewReader(r)
	var exp int64
	var count uint32
	if err := binary.Read(br, binary.BigEndian, &exp); err != nil {
		return 0, nil, err
	}
	if err := binary.Read(br, binary.BigEndian, &count); err != nil {
		return 0, nil, err
	}
	if count > remoteSignMaxItems {
		return 0, nil, fmt.Errorf("too many signatures: %d", count)
	}
	requests := make([]signatureRequest, count)
	for i := range requests {
		key := make([]byte, sha256.Size)
		if _, err := io.ReadFull(br, key); err != nil {
			return 0, nil, err
		}
		n, err := br.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		id := make([]byte, n)
		if _, err := io.ReadFull(br, id); err != nil {
			return 0, nil, err
		}
		requests[i] = signatureRequest{key: key, imageID: string(id)}
	}
	return exp, requests, nil
}

func requestRemoteSignatures(ctx context.Context, address string, exp int64, requests []signatureRequest) ([][sha256.Size]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+address+"/signatures", bytes.NewReader(encodeSignatureRequest(exp, requests)))
	if err != nil {
		return nil, err
	}
	res, err := remoteSignClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", res.StatusCode)
	}
	signatures := make([][sha256.Size]byte, len(requests))
	for i := range signatures {
		if _, err := io.ReadFull(res.Body, signatures[i][:]); err != nil {
			return nil, err
		}
	}
	return signatures, nil
}

func computeSignatures(exp int64, requests []signatureRequest) [][sha256.Size]byte {
	signatures := make([][sha256.Size]byte, len(requests))
	workers := min(runtime.GOMAXPROCS(0), len(requests))
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := w; i < len(requests); i += workers {
				copy(signatures[i][:], computeImageSignature(requests[i].key, requests[i].imageID, exp))
			}
		}()
	}
	wg.Wait()
	return signatures
}

// signatureServiceHandler signs for the given peer only; it is never exposed
// through nginx and listens on a private address.
func signatureServiceHandler(allowedPeer string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || host != allowedPeer {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/signatures" {
			http.NotFound(w, r)
			return
		}
		exp, requests, err := decodeSignatureRequest(http.MaxBytesReader(w, r.Body, 8<<20))
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		signatures := computeSignatures(exp, requests)
		w.Header().Set("Content-Type", "application/octet-stream")
		for i := range signatures {
			if _, err := w.Write(signatures[i][:]); err != nil {
				return
			}
		}
	})
}

// StartSignatureService serves signatures on address until ctx ends.
func StartSignatureService(ctx context.Context, address, allowedPeer string) {
	server := &http.Server{Addr: address, Handler: signatureServiceHandler(allowedPeer), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("signature service on %s: %v", address, err)
		}
	}()
}
