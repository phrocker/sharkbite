/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package fleetevents

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
)

const cursorVersion = 3

var cursorAdditionalData = []byte("shoal-fleet-event-cursor-v3")

type cursorState struct {
	SubscriptionID []byte
	SubscriberID   string
	Fingerprint    auth.Fingerprint
	Generation     uint64
	NextSequence   uint64
	Frontier       uint64
	ExpiresAt      time.Time
}

type cursorCodec struct {
	aead cipher.AEAD
	ttl  time.Duration
}

func newCursorCodec(key []byte, ttl time.Duration) (cursorCodec, error) {
	if len(key) != sha256.Size {
		return cursorCodec{}, errors.New("fleet events: cursor key must contain exactly 32 bytes")
	}
	if ttl <= 0 || ttl > MaxSubscriptionTTL {
		return cursorCodec{}, errors.New("fleet events: cursor TTL is outside its bound")
	}
	derived := sha256.Sum256(append(
		[]byte("shoal-fleet-event-cursor-aead-v2\x00"), key...))
	block, err := aes.NewCipher(derived[:])
	if err != nil {
		return cursorCodec{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return cursorCodec{}, err
	}
	return cursorCodec{aead: aead, ttl: ttl}, nil
}

func (c cursorCodec) seal(state cursorState) (string, error) {
	if err := validateID("cursor subscription ID", state.SubscriptionID, false); err != nil {
		return "", err
	}
	if state.SubscriberID == "" || state.Generation == 0 || state.NextSequence == 0 ||
		state.ExpiresAt.IsZero() || state.ExpiresAt.Location() != time.UTC {
		return "", ErrCursorInvalid
	}
	var body bytes.Buffer
	writeCursorBytes(&body, state.SubscriptionID)
	writeCursorBytes(&body, []byte(state.SubscriberID))
	body.Write(state.Fingerprint[:])
	_ = binary.Write(&body, binary.BigEndian, state.Generation)
	_ = binary.Write(&body, binary.BigEndian, state.NextSequence)
	_ = binary.Write(&body, binary.BigEndian, state.Frontier)
	_ = binary.Write(&body, binary.BigEndian, state.ExpiresAt.UnixNano())
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	encoded := make([]byte, 1, 1+len(nonce)+len(body.Bytes())+c.aead.Overhead())
	encoded[0] = cursorVersion
	encoded = append(encoded, nonce...)
	encoded = c.aead.Seal(encoded, nonce, body.Bytes(), cursorAdditionalData)
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func (c cursorCodec) open(value string, now time.Time) (cursorState, error) {
	encoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(encoded) < 1+c.aead.NonceSize()+c.aead.Overhead() ||
		encoded[0] != cursorVersion {
		return cursorState{}, ErrCursorInvalid
	}
	nonce := encoded[1 : 1+c.aead.NonceSize()]
	body, err := c.aead.Open(
		nil, nonce, encoded[1+c.aead.NonceSize():], cursorAdditionalData)
	if err != nil {
		return cursorState{}, ErrCursorInvalid
	}
	reader := bytes.NewReader(body)
	subscriptionID, err := readCursorBytes(reader)
	if err != nil {
		return cursorState{}, ErrCursorInvalid
	}
	subscriberID, err := readCursorBytes(reader)
	if err != nil {
		return cursorState{}, ErrCursorInvalid
	}
	var state cursorState
	state.SubscriptionID = subscriptionID
	state.SubscriberID = string(subscriberID)
	if _, err := reader.Read(state.Fingerprint[:]); err != nil {
		return cursorState{}, ErrCursorInvalid
	}
	var expires int64
	if binary.Read(reader, binary.BigEndian, &state.Generation) != nil ||
		binary.Read(reader, binary.BigEndian, &state.NextSequence) != nil ||
		binary.Read(reader, binary.BigEndian, &state.Frontier) != nil ||
		binary.Read(reader, binary.BigEndian, &expires) != nil ||
		reader.Len() != 0 {
		return cursorState{}, ErrCursorInvalid
	}
	state.ExpiresAt = time.Unix(0, expires).UTC()
	if !now.Before(state.ExpiresAt) || state.Generation == 0 || state.NextSequence == 0 {
		return cursorState{}, ErrCursorInvalid
	}
	return state, nil
}

func writeCursorBytes(buffer *bytes.Buffer, value []byte) {
	_ = binary.Write(buffer, binary.BigEndian, uint16(len(value)))
	buffer.Write(value)
}

func readCursorBytes(reader *bytes.Reader) ([]byte, error) {
	var size uint16
	if err := binary.Read(reader, binary.BigEndian, &size); err != nil ||
		size == 0 || int(size) > reader.Len() {
		return nil, ErrCursorInvalid
	}
	result := make([]byte, size)
	_, err := reader.Read(result)
	return result, err
}
