// Copyright (c) 2015-2021 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package hash

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"net/http"

	"github.com/minio/minio/internal/etag"
	"github.com/minio/minio/internal/hash/sha256"
	"github.com/minio/minio/internal/ioutil"
)

// A Reader wraps an io.Reader and computes the MD5 checksum
// of the read content as ETag. Optionally, it also computes
// the SHA256 checksum of the content.
//
// If the reference values for the ETag and content SHA26
// are not empty then it will check whether the computed
// match the reference values.
type Reader struct {
	src       io.Reader
	bytesRead int64

	size       int64
	actualSize int64

	checksum      etag.ETag
	contentSHA256 []byte

	// Content checksum
	contentHash   Checksum
	contentHasher hash.Hash

	trailer http.Header

	sha256 hash.Hash
}

// NewReader returns a new Reader that wraps src and computes
// MD5 checksum of everything it reads as ETag.
//
// It also computes the SHA256 checksum of everything it reads
// if sha256Hex is not the empty string.
//
// If size resp. actualSize is unknown at the time of calling
// NewReader then it should be set to -1.
//
// NewReader may try merge the given size, MD5 and SHA256 values
// into src - if src is a Reader - to avoid computing the same
// checksums multiple times.
func NewReader(src io.Reader, size int64, md5Hex, sha256Hex string, actualSize int64) (*Reader, error) {
	MD5, err := hex.DecodeString(md5Hex)
	if err != nil {
		return nil, BadDigest{ // TODO(aead): Return an error that indicates that an invalid ETag has been specified
			ExpectedMD5:   md5Hex,
			CalculatedMD5: "",
		}
	}
	SHA256, err := hex.DecodeString(sha256Hex)
	if err != nil {
		return nil, SHA256Mismatch{ // TODO(aead): Return an error that indicates that an invalid Content-SHA256 has been specified
			ExpectedSHA256:   sha256Hex,
			CalculatedSHA256: "",
		}
	}

	// Merge the size, MD5 and SHA256 values if src is a Reader.
	// The size may be set to -1 by callers if unknown.
	if r, ok := src.(*Reader); ok {
		if r.bytesRead > 0 {
			return nil, errors.New("hash: already read from hash reader")
		}
		if len(r.checksum) != 0 && len(MD5) != 0 && !etag.Equal(r.checksum, MD5) {
			return nil, BadDigest{
				ExpectedMD5:   r.checksum.String(),
				CalculatedMD5: md5Hex,
			}
		}
		if len(r.contentSHA256) != 0 && len(SHA256) != 0 && !bytes.Equal(r.contentSHA256, SHA256) {
			return nil, SHA256Mismatch{
				ExpectedSHA256:   hex.EncodeToString(r.contentSHA256),
				CalculatedSHA256: sha256Hex,
			}
		}
		if r.size >= 0 && size >= 0 && r.size != size {
			return nil, ErrSizeMismatch{Want: r.size, Got: size}
		}

		r.checksum = MD5
		r.contentSHA256 = SHA256
		if r.size < 0 && size >= 0 {
			r.src = etag.Wrap(ioutil.HardLimitReader(r.src, size), r.src)
			r.size = size
		}
		if r.actualSize <= 0 && actualSize >= 0 {
			r.actualSize = actualSize
		}
		return r, nil
	}

	if size >= 0 {
		r := ioutil.HardLimitReader(src, size)
		if _, ok := src.(etag.Tagger); !ok {
			src = etag.NewReader(r, MD5)
		} else {
			src = etag.Wrap(r, src)
		}
	} else if _, ok := src.(etag.Tagger); !ok {
		src = etag.NewReader(src, MD5)
	}
	var h hash.Hash
	if len(SHA256) != 0 {
		h = sha256.New()
	}
	return &Reader{
		src:           src,
		size:          size,
		actualSize:    actualSize,
		checksum:      MD5,
		contentSHA256: SHA256,
		sha256:        h,
	}, nil
}

// ErrInvalidChecksum is returned when an invalid checksum is provided in headers.
var ErrInvalidChecksum = errors.New("invalid checksum")

// AddChecksum will add checksum checks as specified in
// https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity.html
// Returns ErrInvalidChecksum if a problem with the checksum is found.
func (r *Reader) AddChecksum(req *http.Request, ignoreValue bool) error {
	cs, err := GetContentChecksum(req)
	if err != nil {
		return ErrInvalidChecksum
	}
	if cs == nil {
		return nil
	}
	r.contentHash = *cs
	if cs.Type.Trailing() {
		r.trailer = req.Trailer
	}
	if ignoreValue {
		// Do not validate, but allow for transfer
		return nil
	}

	r.contentHasher = cs.Type.Hasher()
	if r.contentHasher == nil {
		return ErrInvalidChecksum
	}
	return nil
}

func (r *Reader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	r.bytesRead += int64(n)
	if r.sha256 != nil {
		r.sha256.Write(p[:n])
	}
	if r.contentHasher != nil {
		r.contentHasher.Write(p[:n])
	}

	if err == io.EOF { // Verify content SHA256, if set.
		if r.sha256 != nil {
			if sum := r.sha256.Sum(nil); !bytes.Equal(r.contentSHA256, sum) {
				return n, SHA256Mismatch{
					ExpectedSHA256:   hex.EncodeToString(r.contentSHA256),
					CalculatedSHA256: hex.EncodeToString(sum),
				}
			}
		}
		if r.contentHasher != nil {
			if r.contentHash.Type.Trailing() {
				var err error
				r.contentHash.Encoded = r.trailer.Get(r.contentHash.Type.Key())
				r.contentHash.Raw, err = base64.StdEncoding.DecodeString(r.contentHash.Encoded)
				if err != nil || len(r.contentHash.Raw) == 0 {
					return 0, ChecksumMismatch{Got: r.contentHash.Encoded}
				}
			}
			if sum := r.contentHasher.Sum(nil); !bytes.Equal(r.contentHash.Raw, sum) {
				err := ChecksumMismatch{
					Want: r.contentHash.Encoded,
					Got:  base64.StdEncoding.EncodeToString(sum),
				}
				return n, err
			}
		}
	}
	if err != nil && err != io.EOF {
		if v, ok := err.(etag.VerifyError); ok {
			return n, BadDigest{
				ExpectedMD5:   v.Expected.String(),
				CalculatedMD5: v.Computed.String(),
			}
		}
	}
	return n, err
}

// Size returns the absolute number of bytes the Reader
// will return during reading. It returns -1 for unlimited
// data.
func (r *Reader) Size() int64 { return r.size }

// ActualSize returns the pre-modified size of the object.
// DecompressedSize - For compressed objects.
func (r *Reader) ActualSize() int64 { return r.actualSize }

// ETag returns the ETag computed by an underlying etag.Tagger.
// If the underlying io.Reader does not implement etag.Tagger
// it returns nil.
func (r *Reader) ETag() etag.ETag {
	if t, ok := r.src.(etag.Tagger); ok {
		return t.ETag()
	}
	return nil
}

// MD5 returns the MD5 checksum set as reference value.
//
// It corresponds to the checksum that is expected and
// not the actual MD5 checksum of the content.
// Therefore, refer to MD5Current.
func (r *Reader) MD5() []byte {
	return r.checksum
}

// MD5Current returns the MD5 checksum of the content
// that has been read so far.
//
// Calling MD5Current again after reading more data may
// result in a different checksum.
func (r *Reader) MD5Current() []byte {
	return r.ETag()[:]
}

// SHA256 returns the SHA256 checksum set as reference value.
//
// It corresponds to the checksum that is expected and
// not the actual SHA256 checksum of the content.
func (r *Reader) SHA256() []byte {
	return r.contentSHA256
}

// MD5HexString returns a hex representation of the MD5.
func (r *Reader) MD5HexString() string {
	return hex.EncodeToString(r.checksum)
}

// MD5Base64String returns a hex representation of the MD5.
func (r *Reader) MD5Base64String() string {
	return base64.StdEncoding.EncodeToString(r.checksum)
}

// SHA256HexString returns a hex representation of the SHA256.
func (r *Reader) SHA256HexString() string {
	return hex.EncodeToString(r.contentSHA256)
}

// ContentCRCType returns the content checksum type.
func (r *Reader) ContentCRCType() ChecksumType {
	return r.contentHash.Type
}

// ContentCRC returns the content crc if set.
func (r *Reader) ContentCRC() map[string]string {
	if r.contentHash.Type == ChecksumNone || !r.contentHash.Valid() {
		return nil
	}
	if r.contentHash.Type.Trailing() {
		return map[string]string{r.contentHash.Type.String(): r.trailer.Get(r.contentHash.Type.Key())}
	}
	return map[string]string{r.contentHash.Type.String(): r.contentHash.Encoded}
}

var _ io.Closer = (*Reader)(nil) // compiler check

// Close and release resources.
func (r *Reader) Close() error { return nil }
