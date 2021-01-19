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

#include "data/streaming/OutputStream.h"
#include "data/streaming/ByteOutputStream.h"
#include "data/streaming/input/InputStream.h"
#include "data/streaming/Streams.h"
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
  explicit IndexBlock(int version);

  virtual ~IndexBlock();

  std::shared_ptr<SerializedIndex> getIndex();

  uint64_t read(cclient::data::streams::InputStream *in);

  std::shared_ptr<KeyIndex> getKeyIndex();

  int getLevel() ;

  int getOffset();

  bool hasNextKey();

}
;

}
}

#endif /* INCLUDE_DATA_CONSTRUCTS_RFILE_META_INDEXBLOCK_H_ */
