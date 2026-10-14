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

package interaction

import "errors"

type committedRecordError struct {
	cause error
}

func (e *committedRecordError) Error() string {
	return e.cause.Error()
}

func (e *committedRecordError) Unwrap() error {
	return e.cause
}

// MarkCommittedRecord marks an error detected after a ResultSink accepted the
// durable interaction. Callers must reconcile or retry the same stable ID; the
// error must never be interpreted as rollback.
func MarkCommittedRecord(err error) error {
	if err == nil || IsCommittedRecord(err) {
		return err
	}
	return &committedRecordError{cause: err}
}

// IsCommittedRecord reports whether an error occurred after durable
// interaction acceptance.
func IsCommittedRecord(err error) bool {
	var marked *committedRecordError
	return errors.As(err, &marked)
}
