/*
 *  Copyright (c) 2017, https://github.com/nebulaim
 *  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package mtproto

import (
	"io"
	net2 "github.com/nebulaim/telegramd/net"
	"fmt"
	"encoding/binary"
	"net"
	"github.com/golang/glog"
	"errors"
)

const (
	CODEC_UNKNOWN = iota
	CODEC_CONNECTED
	CODEC_req_pq
	CODEC_resPQ
	CODEC_req_DH_params
	CODEC_server_DH_params_ok
	CODEC_server_DH_params_fail
	CODEC_set_client_DH_params
	CODEC_dh_gen_ok
	CODEC_dh_gen_retry
	CODEC_dh_gen_fail
	CODEC_AUTH_KEY_OK
	CODEC_ERROR
)

func NewMTProto() *MTProto {
	return &MTProto{
	}
}

type MTProto struct {
}

type AuthKeyStorager interface {
	GetAuthKey(int64) []byte
	PutAuthKey(int64, []byte) error
}

func (m *MTProto) NewCodec(rw io.ReadWriter) (net2.Codec, error) {
	codec := &MTProtoCodec{}
	codec.rw, _ = rw.(io.ReadWriteCloser)
	codec.State = CODEC_CONNECTED
	return codec, nil
}

type MTProtoCodec struct {
	rw io.ReadWriteCloser

	// 缓存AuthKey
	AuthKeyStorager

	State int

	//
	AuthKeyId int64
	AuthKey []byte

	Salt int64
	SessionId int64
	SeqNo int32
}

func (m *MTProtoCodec) RemoteAddr() (net.Addr) {
	return m.rw.(net.Conn).RemoteAddr()
}

func (m *MTProtoCodec) LocalAddr() (net.Addr) {
	return m.rw.(net.Conn).LocalAddr()
}

func (m *MTProtoCodec) Receive() (interface{}, error) {
	var size int
	var n int
	var err error

	b := make([]byte, 1)
	n, err = io.ReadFull(m.rw, b)
	if err != nil {
		return nil, err
	}

	needAck := bool(b[0] >> 7 == 1)

	b[0] = b[0] & 0x7f
	if b[0] < 0xf7 {
		size = int(b[0]) << 2
	} else {
		b := make([]byte, 3)
		n, err = io.ReadFull(m.rw, b)
		if err != nil {
			return nil, err
		}
		size = (int(b[0]) | int(b[1])<<8 | int(b[2])<<16) << 2
	}

	left := size
	buf := make([]byte, size)
	for left > 0 {
		n, err = io.ReadFull(m.rw, buf[size-left:])
		if err != nil {
			return nil, err
		}
		left -= n
	}

	// 截断QuickAck消息，客户端有问题
	if size == 4 {
		// glog.Errorf(("Server response error: ", int32(binary.LittleEndian.Uint32(buf)))
		return nil, fmt.Errorf("Recv QuickAckMessage, ignore!!!! sessionId: ", m.SessionId, ", by client ", m.RemoteAddr())
	}

	authKeyId := int64(binary.LittleEndian.Uint64(buf))
	if authKeyId == 0 {
		// glog.Info("Recv authKeyId is 0")
		var message = &UnencryptedMessage{}
		// glog.Info("Recv authKeyId is 1")
		message.NeedAck = needAck
		err = message.decode(buf[8:])
		// glog.Info("UnencryptedMessage decode ended!")
		if err != nil {
			return nil, err
		}
		// glog.Info("Recv authKeyId is 3", message)
		return message, nil
	} else {
		// glog.Info("Recv authKeyId not 0")

		// TODO(@benqi): 检查m.State状态，authKeyId不为0时codec状态必须是CODEC_AUTH_KEY_OK或CODEC_resPQ
		if m.State != CODEC_AUTH_KEY_OK && m.State != CODEC_resPQ && m.State != CODEC_dh_gen_ok {
			// 连接需要断开
			return nil, fmt.Errorf("Invalid state, is CODEC_AUTH_KEY_OK or CODEC_resPQ, but is %d", m.State)
		}
		if m.AuthKeyId == 0 {
			key := m.GetAuthKey(authKeyId)
			if key == nil {
				return nil, fmt.Errorf("Can't find authKey by authKeyId %d", authKeyId)
			}
			m.AuthKeyId = authKeyId
			m.AuthKey = key
			// glog.Info("Found key, keyId: ", authKeyId, ", key: ", hex.EncodeToString(key))
		} else if m.AuthKeyId != authKeyId {
			return nil, fmt.Errorf("Key error, cacheKey is ", m.AuthKeyId, ", recved keyId is ", authKeyId)
		}

		var message = &EncryptedMessage2{}
		err = message.decode(m.AuthKey, buf[8:])
		if err != nil {
			glog.Error("Decode encrypted message error: ", err)
			return nil, err
		}

		if m.State != CODEC_AUTH_KEY_OK {
			// m.SessionId = message.SessionId
			m.State = CODEC_AUTH_KEY_OK
		}

		return message, nil
	}
}

func (m *MTProtoCodec) Send(msg interface{}) error {
	switch msg.(type) {
	case *UnencryptedMessage:
		b, _ := msg.(*UnencryptedMessage).encode()

		sb := make([]byte, 4)
		// minus padding
		size := len(b)/4

		if size < 127 {
			sb = []byte{byte(size)}
		} else {
			binary.LittleEndian.PutUint32(sb, uint32(size<<8|127))
		}

		b = append(sb, b...)
		_, err := m.rw.Write(b)
		if err != nil {
			glog.Warning("MTProtoCodec - Send UnencryptedMessage errror: %s", err)
			return err
		}
		return nil

	case *EncryptedMessage2:
		encrypedMessage, _ := msg.(*EncryptedMessage2)
		encrypedMessage.SessionId = m.SessionId
		encrypedMessage.salt = m.Salt
		m.SeqNo = m.SeqNo + 1
		encrypedMessage.SeqNo = m.SeqNo
		b, _ := encrypedMessage.encode(int64(m.AuthKeyId), m.AuthKey)

		sb := make([]byte, 4)
		// minus padding
		size := len(b)/4

		if size < 127 {
			sb = []byte{byte(size)}
		} else {
			binary.LittleEndian.PutUint32(sb, uint32(size<<8|127))
		}

		b = append(sb, b...)
		_, err := m.rw.Write(b)
		if err != nil {
			glog.Warning("MTProtoCodec - Send EncryptedMessage2 errror: %s", err)
			return err
		}
		return nil

		// encryptedMessage := message.(*EncryptedMessage2)
		// encryptedMessage.sessionId = m.SessionId
		// encryptedMessage.salt = m.Salt
		// encryptedMessage.salt = 0
		// b, _ := encryptedMessage.encode(m.AuthKey)
		//
		// _, err := m.rw.Write(b)
		// if err != nil {
		// 	glog.Warning("MTProtoCodec - Send EncryptedMessage2 errror: %s", err)
		//   	return err
		// }
		// return nil
	}

	return errors.New("msg type error, only UnencryptedMessage or EncryptedMessage2, but recv invalid type")
}

func (m *MTProtoCodec) Close() error {
	return m.rw.Close()
}
