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
#ifndef INCLUDE_DATA_CONSTRUCTS_RFILE_META_KEYINDEX_H_
#define INCLUDE_DATA_CONSTRUCTS_RFILE_META_KEYINDEX_H_

#include "../../../streaming/OutputStream.h"
#include "../../../streaming/input/InputStream.h"
#include "../../../streaming/input/ByteInputStream.h"
#include "../../../streaming/input/NetworkOrderInputStream.h"
#include "../../../streaming/Streams.h"
#include "IndexEntry.h"

namespace cclient {
namespace data {

class KeyIndex : cclient::data::streams::StreamInterface {
 public:
  KeyIndex(std::vector<int> offsetList, uint8_t *datums, int dataLength)
      :
      newFormat(false),
      dataLength(dataLength) {
    offsets.insert(offsets.end(), offsetList.begin(), offsetList.end());
    data = new uint8_t[dataLength];
    memcpy_fast(data, datums, dataLength);
  }

  virtual ~KeyIndex() {
    if (NULL != data) {
      delete[] data;
    }
  }

  std::shared_ptr<Key> get(uint64_t index);

  int binary_search(const std::shared_ptr<Key> &search_key);

  int binary_search(int first, int last, const std::shared_ptr<Key> &search_key);

 protected:
  int currentValue = 0;
  std::vector<int> offsets;
  uint8_t *data;
  uint32_t dataLength;
  bool newFormat;
};

}
}

#endif /* INCLUDE_DATA_CONSTRUCTS_RFILE_META_KEYINDEX_H_ */
