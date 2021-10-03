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

#include "data/constructs/rfile/meta/KeyIndex.h"

namespace cclient {
namespace data {

std::shared_ptr<Key> KeyIndex::get(uint64_t index) {
  uint64_t len = 0;
  if (index == offsets.size() - 1) {
    len = dataLength - offsets.at(index);
  } else {
    len = offsets.at(index + 1) - offsets.at(index);
  }
  std::shared_ptr<Key> returnKey = std::make_shared<Key>();

  cclient::data::streams::EndianInputStream *inputStream =
      new cclient::data::streams::EndianInputStream(
          (char *)data + offsets.at(index), len);
  returnKey->read(inputStream);
  delete inputStream;

  return returnKey;
}

int KeyIndex::binary_search(const std::shared_ptr<Key> &search_key) {
  return binary_search(0, offsets.size() - 1, search_key);
}

int KeyIndex::binary_search(int first, int last,
                            const std::shared_ptr<Key> &search_key) {
  int low = first;
  int high = last;

  while (low <= high) {
    int mid = ((unsigned int)low + (unsigned int)high) >> 1;

    std::shared_ptr<Key> midVal = get(mid);

    int cmp = midVal->compare(search_key);

    if (cmp < 0)

      low = mid + 1;

    else if (cmp > 0)

      high = mid - 1;

    else

      return mid;  // key found
  }

  return -(low + 1);  // key not found

}  // end binarySear

}  // namespace data

}  // namespace cclient
