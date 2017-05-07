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

#include "../../../streaming/OutputStream.h"
#include "../../../streaming/input/InputStream.h"
#include "../../../streaming/input/ByteInputStream.h"
#include "../../../streaming/input/NetworkOrderInputStream.h"
#include "../../../streaming/Streams.h"
#include "IndexEntry.h"
#include "BaseMetaBlock.h"

namespace cclient
{
namespace data
{

class SerializedIndex : public cclient::data::streams::StreamInterface, public std::iterator<
    std::forward_iterator_tag, IndexEntry>
{
public:
    SerializedIndex (std::vector<int> offsetList, uint8_t *datums, uint32_t dataLength,
                     bool newFormat) :
        newFormat (newFormat), dataLength (
            dataLength), currentPosition (0), ptr (NULL), blockParty (NULL)
    {
        offsets = new std::vector<int>();
        offsets->insert(offsets->end(),offsetList.begin(),offsetList.end());

        data = new uint8_t[dataLength];
        memcpy(data,datums,dataLength);
    }

    SerializedIndex( BaseMetaBlock *source,BaseMetaBlock *block) :
        data (NULL), dataLength (0), newFormat (true), blockParty (block)
    {

        currentPosition = block->getCurrentPosition ();
        ptr = dynamic_cast<SerializedIndex*> (block->getBlock ());

        offsets = new std::vector<int>();

        if (source != block)
        {

            delete source;
        }

    }


    SerializedIndex (BaseMetaBlock *block) :
        data (NULL), dataLength (0), newFormat (true), blockParty (block)
    {
        currentPosition = block->getCurrentPosition ();
        ptr = dynamic_cast<SerializedIndex*> (block->getBlock ());

        offsets = new std::vector<int>();

    }

    virtual
    ~SerializedIndex ()
    {
        if (NULL != data)
        {
            delete[] data;
        }
        if (NULL != ptr)
        {
            delete ptr;
        }
        delete offsets;
    }

    size_t
    size ()
    {
        if (NULL != ptr)
        {
            return ptr->size ();
        }
        return offsets->size ();
    }

    IndexEntry *
    get (uint64_t index)
    {

        if (NULL != ptr)
        {

            return ptr->get (index);
        }
        int len = 0;

        //if (index == offsets->size () - 1)
        //{
        len = dataLength - offsets->at (index);
        /*  }
        else
          {
            len = offsets->at (index + 1) - offsets->at (index);
          }*/
        IndexEntry *returnKey = new IndexEntry (newFormat);

        cclient::data::streams::EndianInputStream *inputStream = new cclient::data::streams::EndianInputStream (
            (char*) data + offsets->at (index), len);
        returnKey->read (inputStream);
        delete inputStream;

        return returnKey;
    }

    IndexEntry *
    get ()
    {
        return get (currentPosition);
    }

    SerializedIndex*
    begin ()
    {
        return this;
    }

    SerializedIndex
    end ()
    {
        return SerializedIndex (true, ptr->offsets->size ());
    }

    IndexEntry *
    operator* ()
    {
        return get (currentPosition);
    }

    SerializedIndex&
    operator++ ()
    {
        currentPosition++;
        return *this;
    }

    SerializedIndex&
    operator++ (int t)
    {
        for (int32_t i = 0; i < (t + 1); i++)
        {
            currentPosition++;
        }

        return *this;
    }

    bool
    isEnd ()
    {
        if (ptr != NULL)
            return currentPosition == ptr->offsets->size ();
        else
            return false;
    }

    bool
    hasPrevious ()
    {
        if (ptr == NULL)
            return false;
        return currentPosition > 0;
    }

    bool
    hasNext ()
    {
        if (blockParty == NULL)
            return false;
        if (!ptr->hasNext ())
            return blockParty->hasNextKey ();
        else
            return true;
    }

    IndexEntry *
    getPrevious ()
    {
	if (currentPosition == 0)
	  return get (0);
      else
	  return get (currentPosition - 1);
    }

    uint32_t
    getPreviousIndex ()
    {
        if (NULL == blockParty)
            return 0;
        int offset = blockParty->getOffset ();
        int prev = 0;
        if (NULL != ptr )
            prev = ptr->getPreviousIndex();
        return offset + prev;
    }

    IndexEntry *
    previous ()
    {
        if (!ptr)
          return 0;
        if (!ptr->hasPrevious ())
        {
            blockParty = blockParty->getPreviousBlock ();
            currentPosition = blockParty->getCurrentPosition ();
            ptr = dynamic_cast<SerializedIndex*> (blockParty->getBlock ());
        }
        if (ptr == this)
          return 0;
        return ptr->previous ();
    }

    bool
    operator!= (const SerializedIndex &rhs)
    {
        return !(currentPosition == rhs.currentPosition);
    }

    explicit SerializedIndex (bool isEnd, int maxSize) :
        currentPosition (maxSize), blockParty(0), dataLength(0), data(0), ptr(0),offsets(0),newFormat(false)
    {

    }

protected:

    uint8_t *data;
    uint32_t dataLength;
    bool newFormat;
    BaseMetaBlock *blockParty;
    uint32_t currentPosition;
    SerializedIndex *ptr;

    std::vector<int> *offsets;

};

}
}

#endif /* INCLUDE_DATA_CONSTRUCTS_RFILE_META_SERIALIZEDINDEX_H_ */
