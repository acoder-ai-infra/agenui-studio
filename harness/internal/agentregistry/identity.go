package agentregistry

import (
	"crypto/sha256"
	"encoding/binary"
)

// identityTupleKey 使用长度前缀编码复合身份，字段内容即使包含分隔符也不会串位。
func identityTupleKey(parts ...string) string {
	size := len(parts) * 8
	for _, part := range parts {
		size += len(part)
	}
	encoded := make([]byte, 0, size)
	var length [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		encoded = append(encoded, length[:]...)
		encoded = append(encoded, part...)
	}
	return string(encoded)
}

func identityTupleDigest(parts ...string) []byte {
	sum := sha256.Sum256([]byte(identityTupleKey(parts...)))
	return append([]byte(nil), sum[:]...)
}
