package shadowsocks

import (
	"fmt"
	disk_bloom "github.com/mzz2017/disk-bloom"
	"github.com/mzz2017/softwind/pool"
	"github.com/mzz2017/softwind/protocol"
	"net"
	"net/netip"
	"strconv"
	"time"
)

type UDPConn struct {
	Timeout time.Duration
	net.PacketConn

	metadata   protocol.Metadata
	cipherConf CipherConf
	masterKey  []byte
	bloom      *disk_bloom.FilterGroup
	sg         SaltGenerator
}

func NewUDPConn(conn net.PacketConn, metadata protocol.Metadata, masterKey []byte, bloom *disk_bloom.FilterGroup) (*UDPConn, error) {
	conf := CiphersConf[metadata.Cipher]
	if conf.NewCipher == nil {
		return nil, fmt.Errorf("invalid CipherConf")
	}
	key := make([]byte, len(masterKey))
	copy(key, masterKey)
	sg, err := GetSaltGenerator(masterKey, conf.SaltLen)
	if err != nil {
		return nil, err
	}
	c := &UDPConn{
		PacketConn: conn,
		metadata:   metadata,
		cipherConf: conf,
		masterKey:  key,
		bloom:      bloom,
		sg:         sg,
	}
	return c, nil
}

func (c *UDPConn) Close() error {
	return c.PacketConn.Close()
}

func (c *UDPConn) Read(b []byte) (n int, err error) {
	n, _, err = c.ReadFrom(b)
	return
}

func (c *UDPConn) Write(b []byte) (n int, err error) {
	return 0, net.InvalidAddrError("")
}

func (c *UDPConn) RemoteAddr() net.Addr {
	return nil
}

func (c *UDPConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	metadata := Metadata{
		Metadata: c.metadata,
	}
	prefix := metadata.BytesFromPool()
	defer pool.Put(prefix)
	chunk := pool.Get(len(prefix) + len(b))
	defer pool.Put(chunk)
	copy(chunk, prefix)
	copy(chunk[len(prefix):], b)
	salt := c.sg.Get()
	toWrite, err := EncryptUDPFromPool(Key{
		CipherConf: c.cipherConf,
		MasterKey:  c.masterKey,
	}, chunk, salt)
	pool.Put(salt)
	if err != nil {
		return 0, err
	}
	defer pool.Put(toWrite)
	if c.bloom != nil {
		c.bloom.ExistOrAdd(toWrite[:c.cipherConf.SaltLen])
	}
	return c.PacketConn.WriteTo(toWrite, addr)
}

func (c *UDPConn) ReadFrom(b []byte) (n int, addr net.Addr, err error) {
	n, addr, err = c.PacketConn.ReadFrom(b)
	if err != nil {
		return 0, nil, err
	}
	enc := pool.Get(len(b))
	defer pool.Put(enc)
	copy(enc, b)
	n, err = DecryptUDP(Key{
		CipherConf: c.cipherConf,
		MasterKey:  c.masterKey,
	}, b[:n])
	if err != nil {
		return
	}
	if c.bloom != nil {
		if exist := c.bloom.ExistOrAdd(enc[:c.cipherConf.SaltLen]); exist {
			err = protocol.ErrReplayAttack
			return
		}
	}
	// parse sAddr from metadata
	sizeMetadata, err := BytesSizeForMetadata(b)
	if err != nil {
		return 0, nil, err
	}
	mdata, err := NewMetadata(b)
	if err != nil {
		return 0, nil, err
	}
	var typ protocol.MetadataType
	switch typ {
	case protocol.MetadataTypeIPv4, protocol.MetadataTypeIPv6:
		ipport, err := netip.ParseAddrPort(net.JoinHostPort(mdata.Hostname, strconv.Itoa(int(mdata.Port))))
		if err != nil {
			return 0, nil, err
		}
		addr = net.UDPAddrFromAddrPort(ipport)
	}
	copy(b, b[sizeMetadata:])
	n -= sizeMetadata
	return n, addr, nil
}
