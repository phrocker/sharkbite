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
#ifndef INCLUDE_DATA_CONSTRUCTS_RFILE_META_INDEXBLOCK_H_
#define INCLUDE_DATA_CONSTRUCTS_RFILE_META_INDEXBLOCK_H_

#include "../../../streaming/OutputStream.h"
#include "../../../streaming/ByteOutputStream.h"
#include "../../../streaming/input/InputStream.h"
#include "../../../streaming/Streams.h"
#include "IndexEntry.h"
#include "SerializedIndex.h"
#include "KeyIndex.h"
#include "IndexMetaBlock.h"

#include <iomanip>      // std::setfill, std::setw
namespace cclient {
namespace data {

class IndexBlock : public IndexMetaBlock, public cclient::data::streams::StreamInterface {
 protected:
  int level;
  int offset;
  int version;
  bool hasNext;

  std::shared_ptr<SerializedIndex> index;
  uint8_t *serializedIndex;

  std::shared_ptr<KeyIndex> keyIndex;

 public:
  explicit IndexBlock(int version)
      : level(0),
        offset(0),
        version(version),
        hasNext(false),
        index(
        NULL),
        serializedIndex(
        NULL),
        keyIndex(NULL) {

  }

  virtual ~IndexBlock() {

    if (serializedIndex != 0) {
      delete[] serializedIndex;
    }

  }

  std::shared_ptr<SerializedIndex> getIndex() {
    return index;
  }

  uint64_t read(cclient::data::streams::InputStream *in) {
    if (version == 6 || version == 7 || version == 8) {
      level = in->readInt();
      offset = in->readInt();
      std::cout << " level is " << level << " " << offset << std::endl;
      hasNext = in->readBoolean()==0x01;

      std::cout << "index block is " << (hasNext) << std::endl;

      int numOffsets = in->readInt();
      std::vector<int> offsets;
      offsets.reserve(numOffsets);
      std::cout << "number of offsets " << numOffsets << std::endl;
      for (int i = 0; i < numOffsets; i++) {
        offsets.push_back(in->readInt());
      }

      int indexSize = in->readInt();
      serializedIndex = new uint8_t[indexSize];
      in->readBytes(serializedIndex, indexSize);

      index = std::make_shared<SerializedIndex>(offsets, serializedIndex,
                                                indexSize, true);
      keyIndex = std::make_shared<KeyIndex>(offsets, serializedIndex,
                                            indexSize);
    } else if (version == 3) {
      level = 0;
      offset = 0;
      hasNext = false;
      int size = in->readInt();

      cclient::data::streams::ByteOutputStream *byteOutStream =
          new cclient::data::streams::ByteOutputStream(0);
      cclient::data::streams::DataOutputStream *outStream =
          new cclient::data::streams::DataOutputStream(byteOutStream);
      std::vector<int> offsetList;
      for (int i = 0; i < size; i++) {
        IndexEntry entry;
        offsetList.push_back(byteOutStream->getSize());
        entry.read(in);
        entry.write(outStream);

      }
      serializedIndex = new uint8_t[byteOutStream->getSize()];
      byteOutStream->getByteArray((char*) serializedIndex,
                                  byteOutStream->getSize());
      delete outStream;
      delete byteOutStream;

      index = std::make_shared<SerializedIndex>(offsetList, serializedIndex,
                                                byteOutStream->getSize(),
                                                false);
      keyIndex = std::make_shared<KeyIndex>(offsetList, serializedIndex,
                                            byteOutStream->getSize());

    } else if (version == 4) {
      level = 0;
      offset = 0;
      hasNext = false;

      int numIndexEntries = in->readInt();

      std::vector<int> offsets;
      for (int i = 0; i < numIndexEntries; i++) {
        int offset = in->readInt();
        offsets.push_back(offset);
      }

      int size = in->readInt();

      serializedIndex = new uint8_t[size];
      in->readBytes(serializedIndex, size);

      index = std::make_shared<SerializedIndex>(offsets, serializedIndex, size,
                                                false);
      keyIndex = std::make_shared<KeyIndex>(offsets, serializedIndex, size);
    } else {
      std::runtime_error("Unexpected version");
    }

    return in->getPos();
  }

  std::shared_ptr<KeyIndex>
  getKeyIndex() {
    return keyIndex;
  }

  int getLevel() {
    return level;
  }

  int getOffset() {
    return offset;
  }

  bool hasNextKey() {
    return hasNext;
  }

}
;

}
}

#endif /* INCLUDE_DATA_CONSTRUCTS_RFILE_META_INDEXBLOCK_H_ */
