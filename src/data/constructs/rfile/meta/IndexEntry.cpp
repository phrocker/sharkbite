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

#include "data/constructs/rfile/meta/IndexEntry.h"

namespace cclient{
  namespace data{

IndexEntry::IndexEntry (std::shared_ptr<cclient::data::Key> mKey, uint32_t entryCount) :
    entries (entryCount), newFormat (false)
{
	key = std::make_shared<cclient::data::Key>(mKey);
}

IndexEntry::~IndexEntry ()
{
}

  }
  
  
}
  
