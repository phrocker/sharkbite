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

#include "data/constructs/rfile/meta/SerializedIndex.h"



namespace cclient {
namespace data {


  SerializedIndex::SerializedIndex(const std::vector<int> &offsetList, uint8_t *datums, uint32_t dataLength, bool newFormat)
      :
      newFormat(newFormat),
      dataLength(dataLength),
      currentPosition(0),
      ptr(NULL),
      blockParty(NULL) {
    offsets.insert(offsets.end(), offsetList.begin(), offsetList.end());

    data = new uint8_t[dataLength];
    memcpy_fast(data, datums, dataLength);
  }

  SerializedIndex::SerializedIndex(const std::shared_ptr<BaseMetaBlock> &source,
          const std::shared_ptr<cclient::data::Key> &key, const std::shared_ptr<BaseMetaBlock> &block)
      :
      data(NULL),
      dataLength(0),
      newFormat(true),
      blockParty(block) {
    currentPosition = block->getCurrentPosition();
    ptr = std::static_pointer_cast<SerializedIndex>(block->getBlock());
    ptr->currentPosition=currentPosition;
    keyRef = key;
  }

  SerializedIndex::SerializedIndex(const std::shared_ptr<BaseMetaBlock> &block)
      :
      data(NULL),
      dataLength(0),
      newFormat(true),
      blockParty(block) {
    currentPosition = block->getCurrentPosition();
    ptr = std::static_pointer_cast<SerializedIndex>(block->getBlock());
    ptr->currentPosition=currentPosition;
  }

  std::shared_ptr<IndexEntry> SerializedIndex::get(uint64_t index) {
    if ((offsets.empty()) && NULL != ptr) {
      return ptr->get(index);
    }
    int len = 0;

    if (index >= offsets.size() )
      return nullptr;

          

    len = dataLength - offsets.at(index);
    
    std::shared_ptr<IndexEntry> returnKey = std::make_shared<IndexEntry>(newFormat);

    cclient::data::streams::EndianInputStream inputStream((char*) data + offsets.at(index), len);

    returnKey->read(&inputStream);

    return returnKey;
  }


   SerializedIndex&
  SerializedIndex::operator++() {
    // if we don't have offsets, then use ptr
    // if we do have offsets, then we should increment
    if (offsets.size() == 0) {
      if (ptr->currentPosition + 1 < ptr->offsets.size()) {
        ptr->currentPosition++;
        return *this;
      } else {
        blockParty = blockParty->getNextBlock();
        currentPosition = blockParty->getCurrentPosition();
        ptr = std::static_pointer_cast<SerializedIndex>(blockParty->getBlock());
        ptr->currentPosition=currentPosition;
        return *this;
      }
    } else {
      currentPosition++;
    }
    return *this;
  }

  SerializedIndex&
  SerializedIndex::operator++(int t) {
    // if we don't have offsets, then use ptr
    // if we do have offsets, then we should increment
    if (offsets.size() == 0) {
      if (ptr->currentPosition + 1 < ptr->offsets.size()) {
        for (int32_t i = 0; i < (t + 1); i++) {
          ptr->currentPosition++;
        }
        return *this;
      } else {
        blockParty = blockParty->getNextBlock();
        currentPosition = blockParty->getCurrentPosition();
        ptr = std::static_pointer_cast<SerializedIndex>(blockParty->getBlock());
        ptr->currentPosition=currentPosition;
      }
    } else {
      for (int32_t i = 0; i < (t + 1); i++) {
        currentPosition++;
      }
    }
    return *this;
  }

  bool SerializedIndex::isEnd() {
    if (ptr != NULL)
      return currentPosition == ptr->offsets.size();
    else
      return false;
  }

  bool SerializedIndex::hasNextPosition() {
    if (ptr != NULL)
      return currentPosition + 1 < ptr->offsets.size();
    else
      return false;
  }

  bool SerializedIndex::hasPrevious() {
    if (ptr == NULL)
      return false;
    return currentPosition > 0;
  }

  bool SerializedIndex::hasNext() {
    if ((nullptr == blockParty && offsets.empty()))
      return false;

    if (offsets.empty()) {
      if (ptr) {
        auto morev = ptr->hasNext();
        if (!morev) {
          morev = blockParty->getIndexBlock()->hasNextKey();
        }

        return morev;
      } else {
        return false;
      }
    } else {
      if (currentPosition + 1 >= offsets.size()) {
        if (!blockParty) {
          return false;
        }
        return blockParty->getIndexBlock()->hasNextKey();
      }

    }
    return true;
  }

  std::shared_ptr<IndexEntry> SerializedIndex::getPrevious() {
    if (currentPosition == 0)
      return get(0);
    else
      return get(currentPosition - 1);
  }

  uint32_t SerializedIndex::getPreviousIndex() {
    if (NULL == blockParty)
      return 0;
    int offset = blockParty->getOffset();
    int prev = 0;
    if (NULL != ptr)
      prev = ptr->getPreviousIndex();
    return offset + prev;
  }

  std::shared_ptr<IndexEntry> SerializedIndex::previous() {
    if (!ptr)
      return 0;
    if (!ptr->hasPrevious()) {
      blockParty = blockParty->getPreviousBlock();
      currentPosition = blockParty->getCurrentPosition();
      ptr = std::static_pointer_cast<SerializedIndex>(blockParty->getBlock());
    }
    if (ptr == shared_from_this())
      return 0;
    return ptr->previous();
  }

}

}