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

#include "data/constructs/rfile/meta/Block.h"

namespace cclient {
namespace data {

std::shared_ptr<Block> Block::lookup(const std::shared_ptr<Key> &key) {
  int64_t posCheck = indexBlock->getKeyIndex()->binary_search(key);
  if (posCheck < 0) {
    posCheck = (posCheck * -1) - 1;
  }
  uint64_t pos = posCheck;
  if (pos >= indexBlock->getIndex()->size()) {
    if (parent == NULL) { /******** ******** ***** */
      throw std::runtime_error("Illegal state ( parent is null )");
    }

    currentPosition = pos;
    return shared_from_this();
  }

  currentPosition = pos;
  if (indexBlock->getLevel() == 0) {
    return shared_from_this();
  }

  std::shared_ptr<IndexEntry> ie = indexBlock->getIndex()->get(pos);
  std::shared_ptr<Block> newChild =
      std::make_shared<Block>(shared_from_this(), getIndexBlock(ie));

  std::shared_ptr<Block> returnBlock = newChild->lookup(key);

  return returnBlock;
}
}  // namespace data

}  // namespace cclient
