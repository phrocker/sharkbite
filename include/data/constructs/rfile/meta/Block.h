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
#ifndef INCLUDE_DATA_CONSTRUCTS_RFILE_META_BLOCK_H_
#define INCLUDE_DATA_CONSTRUCTS_RFILE_META_BLOCK_H_

#include "BaseMetaBlock.h"
#include "IndexBlock.h"
#include "BlockLookup.h"
#include "../../Key.h"

namespace cclient
{
namespace data
{

class Block : public BaseMetaBlock
{
protected:
    Block *parent;
    IndexBlock *indexBlock;
    uint32_t currentPosition;
    BlockLookup *blockStore;

public:

    Block (BlockLookup *blockStore, Block *parent, IndexBlock *block) :
        blockStore (blockStore), parent (parent), indexBlock (block), currentPosition (
            0)
    {

    }

    Block (BlockLookup *blockStore, IndexBlock *block) :
        Block (blockStore, NULL, block)
    {

    }

    Block (Block *blockStore, IndexBlock *block) :
        Block (blockStore->blockStore, blockStore, block)
    {

    }

    ~Block ()
    {
        if (NULL != indexBlock)
        {
            delete indexBlock;
        }
    }

    uint32_t
    getCurrentPosition ()
    {
        return currentPosition;
    }

    IndexBlock *
    getIndexBlock ()
    {
        return indexBlock;
    }

    cclient::data::streams::StreamInterface *
    getBlock ()
    {
        return indexBlock->getIndex ();
    }

    uint32_t
    getOffset ()
    {
        return indexBlock->getOffset ();
    }

    bool
    hasNextKey ()
    {
        return indexBlock->hasNextKey ();
    }

    IndexBlock *
    getIndexBlock (IndexEntry *ie)
    {
        return blockStore->getIndexBlock (ie);
    }

    Block *
    lookup (Key *key)
    {
        int64_t posCheck = indexBlock->getKeyIndex ()->binary_search (key);
        if (posCheck < 0)
        {
            posCheck = (posCheck * -1) - 1;
        }
        uint64_t pos = posCheck;
        if (pos == indexBlock->getIndex ()->size ())
        {
            if (parent != NULL)
            {
                throw std::runtime_error ("Illegal state");
            }
            currentPosition = pos;
            return this;
        }

        currentPosition = pos;
        if (indexBlock->getLevel () == 0)
        {
            return this;
        }

        IndexEntry *ie = indexBlock->getIndex ()->get (pos);
        Block *newChild = new Block (this, getIndexBlock (ie));

        Block *returnBlock = newChild->lookup (key);

        delete newChild;
        return returnBlock;

    }

    Block *
    getLast ()
    {
        currentPosition = indexBlock->getIndex ()->size () - 1;
        if (indexBlock->getLevel () == 0)
        {
            return NULL;
        }

        IndexEntry *ie = indexBlock->getIndex ()->get (currentPosition);
        Block *newChild = new Block (this, getIndexBlock (ie));
        Block *returnBlock = newChild->getLast ();
        if (returnBlock != NULL)
            delete newChild;
        else
            returnBlock = newChild;
        return returnBlock;

    }

    Block *
    getPrevious ()
    {
        if (currentPosition == 0)
        {
            return parent->getPrevious ();
        }

        currentPosition--;

        IndexEntry *ie = indexBlock->getIndex ()->get (currentPosition);
        Block *newChild = new Block (this, getIndexBlock (ie));
        Block *returnBlock = newChild->getLast ();
        if (returnBlock != NULL)
            delete newChild;
        else
            returnBlock = newChild;
        return returnBlock;

    }

    Block *
    getPreviousBlock ()
    {
        return parent->getPrevious ();
    }

};
}
}
#endif /* INCLUDE_DATA_CONSTRUCTS_RFILE_META_BLOCK_H_ */
