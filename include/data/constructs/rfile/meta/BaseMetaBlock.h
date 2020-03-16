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
#ifndef INCLUDE_DATA_CONSTRUCTS_RFILE_META_BASEMETABLOCK_H_
#define INCLUDE_DATA_CONSTRUCTS_RFILE_META_BASEMETABLOCK_H_
#include "data/streaming/Streams.h"
#include <memory>
#include "IndexMetaBlock.h"

namespace cclient {
namespace data {

class BaseMetaBlock {

 public:

  virtual ~BaseMetaBlock() {
  }

  virtual uint32_t
  getCurrentPosition() = 0;

  virtual std::shared_ptr<data::streams::StreamInterface>
  getBlock() = 0;

  virtual std::shared_ptr<BaseMetaBlock>
  getPreviousBlock() = 0;

  virtual std::shared_ptr<BaseMetaBlock> getNextBlock() = 0;

  virtual std::shared_ptr<BaseMetaBlock> getNext() = 0;

  virtual std::shared_ptr<IndexMetaBlock> getIndexBlock() = 0;

  virtual uint32_t
  getOffset() = 0;

  virtual bool
  hasNextKey() =0;

};

}
}

#endif /* INCLUDE_DATA_CONSTRUCTS_RFILE_META_BASEMETABLOCK_H_ */
