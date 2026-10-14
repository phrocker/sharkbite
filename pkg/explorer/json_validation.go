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

package explorer

import (
	"fmt"
	"unicode/utf8"
)

func validateStrictJSONStringEncoding(data []byte) error {
	for index := 0; index < len(data); {
		if data[index] != '"' {
			index++
			continue
		}
		next, err := scanStrictJSONString(data, index)
		if err != nil {
			return err
		}
		index = next
	}
	return nil
}

func scanStrictJSONString(data []byte, start int) (int, error) {
	index := start + 1
	segmentStart := index
	for index < len(data) {
		switch data[index] {
		case '"':
			if !utf8.Valid(data[segmentStart:index]) {
				return 0, fmt.Errorf("JSON string contains invalid UTF-8")
			}
			return index + 1, nil
		case '\\':
			if !utf8.Valid(data[segmentStart:index]) {
				return 0, fmt.Errorf("JSON string contains invalid UTF-8")
			}
			index++
			if index >= len(data) {
				return 0, fmt.Errorf("JSON string has an incomplete escape")
			}
			switch data[index] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				index++
			case 'u':
				code, next, err := parseJSONHex4(data, index+1)
				if err != nil {
					return 0, err
				}
				index = next
				switch {
				case code >= 0xd800 && code <= 0xdbff:
					if index+2 > len(data) || data[index] != '\\' || data[index+1] != 'u' {
						return 0, fmt.Errorf("JSON string has an unpaired high surrogate")
					}
					low, next, err := parseJSONHex4(data, index+2)
					if err != nil {
						return 0, err
					}
					if low < 0xdc00 || low > 0xdfff {
						return 0, fmt.Errorf("JSON string has an unpaired high surrogate")
					}
					index = next
				case code >= 0xdc00 && code <= 0xdfff:
					return 0, fmt.Errorf("JSON string has an unpaired low surrogate")
				}
			default:
				return 0, fmt.Errorf("JSON string has an invalid escape")
			}
			segmentStart = index
		default:
			if data[index] < 0x20 {
				return 0, fmt.Errorf("JSON string contains an unescaped control character")
			}
			index++
		}
	}
	return 0, fmt.Errorf("JSON string is unterminated")
}

func parseJSONHex4(data []byte, start int) (uint16, int, error) {
	if start+4 > len(data) {
		return 0, 0, fmt.Errorf("JSON string has an incomplete Unicode escape")
	}
	var value uint16
	for _, digit := range data[start : start+4] {
		value <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			value |= uint16(digit - '0')
		case digit >= 'a' && digit <= 'f':
			value |= uint16(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			value |= uint16(digit-'A') + 10
		default:
			return 0, 0, fmt.Errorf("JSON string has an invalid Unicode escape")
		}
	}
	return value, start + 4, nil
}
