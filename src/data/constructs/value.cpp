/*
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

#include "data/constructs/value.h"

#include <utility>

namespace cclient {
namespace data {

Value::Value() : valueSize(0) {
  value = new uint8_t[0];
  offset = 0;
}

Value::~Value() {
  if (value != NULL) delete[] value;
}

/**
 * Sets the value using the value and the corresponding size and offset.
 */
void Value::setValue(uint8_t *val, size_t size, uint32_t ptrOff) {
  if ((size + ptrOff) > valueSize) {
    uint8_t *oldVal = value;
    value = new uint8_t[size + ptrOff];
    if (oldVal != NULL) {
      memcpy(value, oldVal, ptrOff);
      delete[] oldVal;
    }
    valueSize = size + ptrOff;
  }

  if (size > 0) memcpy(value + ptrOff, val, size);

  offset = size;
}

void Value::setValue(const std::string &val) {
  setValue((uint8_t *)val.c_str(), val.size());
}

/**
 * Appends val to the current value.
 */
void Value::append(uint8_t *val, size_t size) {
  if ((size + offset) > valueSize) {
    uint8_t *oldVal = value;
    value = new uint8_t[size + offset];
    memcpy(value, oldVal, offset);
    delete[] oldVal;
    valueSize = size + offset;
  }

  memcpy(value + offset, val, size);

  offset += size;
}

/**
 * Deep copies the object.
 */
void Value::deepCopy(Value *v) {
  v->value = value;
  v->valueSize = valueSize;
  v->offset = offset;
  value = NULL;
  offset = 0;
  valueSize = 0;
}

uint8_t *Value::data() { return value; }

size_t Value::size() { return offset; }

std::shared_ptr<streams::StreamInterface> Value::getStream() {
  return shared_from_this();
}

std::pair<uint8_t *, size_t> Value::getValue() const {
  return std::make_pair(value, valueSize);
}

std::string Value::getValueAsString() const {
  if (valueSize > 0 && value) {
    return std::string((char *)(value), valueSize);
  } else {
    return std::string("");
  }
}

uint64_t Value::write(cclient::data::streams::OutputStream *outStream) {
  outStream->writeInt(valueSize);
  return outStream->writeBytes(value, valueSize);
}

uint64_t Value::read(cclient::data::streams::InputStream *in) {
  if (value != NULL ) delete[] value;

  uint32_t size = in->readInt();
  value = new uint8_t[size];
  valueSize = size;
  return in->readBytes(value, size);
}

bool Value::operator==(const Value &rhs) const {
  if (valueSize == rhs.valueSize && offset == rhs.offset) {
    return (memcmp(value, rhs.value, valueSize) == 0);
  } else
    return false;
}

bool Value::operator!=(const Value &rhs) const { return !(*this == rhs); }
}  // namespace data
}  // namespace cclient
