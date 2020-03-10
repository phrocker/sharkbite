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

#include "data/constructs/rfile/bcfile/BlockRegion.h"
#include "data/streaming/ByteOutputStream.h"

#include <iostream>

namespace cclient {
namespace data {
uint64_t BlockRegion::read(streams::InputStream *in) {
  offset = in->readHadoopLong();
  compressedSize = in->readHadoopLong();
  rawSize = in->readHadoopLong();
  return in->getPos();
}

uint64_t BlockRegion::write(streams::OutputStream *out) {
  cclient::data::streams::BigEndianByteStream *byteStream = dynamic_cast<cclient::data::streams::BigEndianByteStream*>(out);
  uint64_t pos = -1;
  if (byteStream != nullptr) {
    byteStream->writeEncodedLong(offset);
    byteStream->writeEncodedLong(compressedSize);
    pos = byteStream->writeEncodedLong(rawSize);
  } else {
    out->writeEncodedLong(offset);
    out->writeEncodedLong(compressedSize);
    pos = out->writeEncodedLong(rawSize);
  }
  return pos;

}

}
}
