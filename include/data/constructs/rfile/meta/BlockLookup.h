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
#ifndef INCLUDE_DATA_CONSTRUCTS_RFILE_META_BLOCKLOOKUP_H_
#define INCLUDE_DATA_CONSTRUCTS_RFILE_META_BLOCKLOOKUP_H_

#include "IndexBlock.h"
#include "IndexEntry.h"
namespace cclient
{
namespace data
{
class BlockLookup
{
public:
    BlockLookup ()
    {

    }
    virtual
    ~BlockLookup ()
    {

    }
    //virtual std::shared_ptr<IndexBlock> getBlock();

    virtual std::shared_ptr<IndexBlock> getIndexBlock (std::shared_ptr<IndexEntry> ie) = 0;
};
}
}

#endif /* INCLUDE_DATA_CONSTRUCTS_RFILE_META_BLOCKLOOKUP_H_ */
