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
#ifndef INCLUDE_DATA_CONSTRUCTS_RFILE_META_SERIALIZEDINDEX_H_
#define INCLUDE_DATA_CONSTRUCTS_RFILE_META_SERIALIZEDINDEX_H_

#include <vector>
#include <memory>
#include "data/streaming/OutputStream.h"
#include "data/streaming/input/InputStream.h"
#include "data/streaming/input/ByteInputStream.h"
#include "data/streaming/input/NetworkOrderInputStream.h"
#include "data/constructs/Key.h"
#include "data/streaming/Streams.h"
#include "IndexEntry.h"
#include "BaseMetaBlock.h"
#include "utils/MemoryUtils.h"

namespace cclient {
namespace data {

class SerializedIndex : public cclient::data::streams::StreamInterface, public std::iterator<std::forward_iterator_tag, IndexEntry>, public std::enable_shared_from_this<SerializedIndex> {
 public:
  explicit SerializedIndex(const std::vector<int> &offsetList, uint8_t *datums, uint32_t dataLength, bool newFormat);

  explicit SerializedIndex(const std::shared_ptr<BaseMetaBlock> &source,const std::shared_ptr<cclient::data::Key> &key, const std::shared_ptr<BaseMetaBlock> &block);

  explicit SerializedIndex(const std::shared_ptr<BaseMetaBlock> &block);

  virtual ~SerializedIndex() {
    if (NULL != data) {
      delete[] data;
    }
  }

  size_t size() {
    if (NULL != ptr) {
      return ptr->size();
    }
    return offsets.size();
  }

   std::shared_ptr<IndexEntry> get(uint64_t index);

  std::shared_ptr<IndexEntry> get() {
    if (offsets.size() == 0) {
      return get(ptr->currentPosition);
    } else {
      return ptr->operator*();
    }
  }

  std::shared_ptr<SerializedIndex> begin() {
    return shared_from_this();
  }

  std::shared_ptr<SerializedIndex> end() {
    return std::make_shared<SerializedIndex>(true, ptr->offsets.size());
  }

  std::shared_ptr<IndexEntry> operator*() {
    if (offsets.size() == 0) {
      return get(ptr->currentPosition);
    } else {
      return ptr->operator*();
    }
  }

  SerializedIndex&
  operator++();

  SerializedIndex&
  operator++(int t);

  bool isEnd();

  bool hasNextPosition();

  bool hasPrevious();

  bool hasNext();

  std::shared_ptr<IndexEntry> getPrevious();

  uint32_t getPreviousIndex();

  std::shared_ptr<IndexEntry> previous();

  bool operator!=(const SerializedIndex &rhs) {
    return !(currentPosition == rhs.currentPosition);
  }

  explicit SerializedIndex(bool isEnd, int maxSize)
      :
      currentPosition(maxSize),
      blockParty(0),
      dataLength(0),
      data(0),
      ptr(0),
      offsets(0),
      newFormat(false) {

  }

 protected:

  uint8_t *data;
  uint32_t dataLength;
  bool newFormat;
  std::shared_ptr<BaseMetaBlock> blockParty;
  volatile uint32_t currentPosition;
  std::shared_ptr<SerializedIndex> ptr;
  std::vector<int> offsets;
  std::shared_ptr<cclient::data::Key> keyRef = nullptr;

};

}
}

#endif /* INCLUDE_DATA_CONSTRUCTS_RFILE_META_SERIALIZEDINDEX_H_ */
