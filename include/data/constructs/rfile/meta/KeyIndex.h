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

  std::shared_ptr<Key> get(uint64_t index) {
    uint64_t len = 0;
    if (index == offsets.size() - 1) {
      len = dataLength - offsets.at(index);
    } else {
      len = offsets.at(index + 1) - offsets.at(index);
    }
    std::shared_ptr<Key> returnKey = std::make_shared<Key>();

    cclient::data::streams::EndianInputStream *inputStream = new cclient::data::streams::EndianInputStream((char*) data + offsets.at(index), len);
    returnKey->read(inputStream);
    delete inputStream;

    return returnKey;
  }

  int binary_search(const std::shared_ptr<Key> &search_key) {
    return binary_search(0, offsets.size() - 1, search_key);
  }

  int binary_search(int first, int last, const std::shared_ptr<Key> &search_key) {
    int index;

    if (first > last)
      index = -1;

    else {
      int mid = (first + last) / 2;

      std::shared_ptr<Key> midKey = get(mid);
      if (*search_key == *midKey)
        index = mid;
      else

      if (*search_key < *midKey)
        index = binary_search(first, mid - 1, search_key);
      else
        index = binary_search(mid + 1, last, search_key);

    }  // end if
    return index;
  }  // end binarySearch

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
