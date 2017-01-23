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
#ifndef INCLUDE_DATA_CONSTRUCTS_RFILE_META_LOCALITYGROUPREADER_H_
#define INCLUDE_DATA_CONSTRUCTS_RFILE_META_LOCALITYGROUPREADER_H_

#include "../../../streaming/accumulo/StreamSeekable.h"
#include "../../../streaming/OutputStream.h"
#include "../../../streaming/input/InputStream.h"
#include "../../../streaming/Streams.h"
#include "../../../streaming/StreamRelocation.h"
#include "../../../streaming/StreamEnvironment.h"
#include "../../../streaming/accumulo/FileIterator.h"
#include "../../../streaming/DataStream.h"
#include "../../../exceptions/IllegalArgumentException.h"
#include "../../../exceptions/InterationInterruptedException.h"
#include "IndexEntry.h"

#include "../bcfile/BlockCompressedFile.h"
// constructs

#include "../../../constructs/Key.h"
#include "../../../constructs/value.h"
#include "../../../constructs/compressor/compressor.h"
#include "../../../constructs/Range.h"
#include "../../../constructs/SkippedRelativeKey.h"
#include "../../../constructs/rkey.h"

namespace cclient
{
namespace data
{

class LocalityGroupReader : public cclient::data::streams::FileIterator
{
protected:
    BlockCompressedFile *bcFile;
    cclient::data::streams::InputStream *reader;
    int version;
    volatile bool closed;
    bool checkRange;
    volatile bool topExists = false;
    cclient::data::streams::InputStream *currentStream;
    volatile bool interrupted = false;
    Range *currentRange;
    SerializedIndex *iiter;
    Key *prevKey;
    uint32_t entriesLeft;
    IndexManager *index;
    

    
    int blockCount;
    Key *firstKey;
    int startBlock;
    

    

    

    RelativeKey *rKey;

    Value *val;

    

    void
    close ()
    {
        if (NULL != currentStream)
        {
            currentStream->close ();
            delete currentStream;
            currentStream = NULL;
        }
    }

public:
    LocalityGroupReader (BlockCompressedFile *bcFile,
                         cclient::data::streams::InputStream *input_stream,
                         LocalityGroupMetaData *metadata, int version) :
        bcFile (bcFile), reader (input_stream), version (version), closed (
            false), checkRange (false), topExists (false), currentStream (
                NULL), interrupted (false), currentRange (NULL), iiter (NULL), prevKey (
                    NULL), entriesLeft (-1)
    {
        index = metadata->getIndexManager ();
        firstKey = dynamic_cast<Key*> (metadata->getFirstKey ());
        startBlock = metadata->getStartBlock ();
        blockCount = index->getSize ();
        rKey = NULL;
    }

    virtual
    ~LocalityGroupReader ()
    {
        if (NULL != iiter)
            delete iiter;
        if (NULL != firstKey)
        {
            delete firstKey;
        }
        if (NULL != prevKey)
            delete prevKey;
    }

    Key *
    getFirstKey ()
    {
        return firstKey;
    }

    Key *
    getLastKey ()
    {
        return dynamic_cast<Key*> (iiter->get (iiter->size () - 1)->getKey ());
    }

    Key *
    getTopKey ()
    {
        return dynamic_cast<Key*> (rKey->getStream ());
    }

    bool
    hasTop ()
    {
        return topExists;
    }

    void
    seek (cclient::data::streams::StreamRelocation *position)
    {
        cclient::data::streams::StreamSeekable *newSeekRequest =
            dynamic_cast<cclient::data::streams::StreamSeekable*> (position);
        if (closed)
        {
            throw cclient::exceptions::IllegalArgumentException ("Locality group reader closed");
        }

        if (newSeekRequest->getColumnFamilies ()->size () > 0
                || newSeekRequest->isInclusive ())
        {
            throw std::runtime_error (
                "I do not know how to filter column families");
        }

        if (interrupted)
        {
            throw cclient::exceptions::IterationInterruptedException ("interrupted");
        }

        currentRange = newSeekRequest->getRange ();

        checkRange = true;

        if (blockCount == 0)
        {
            topExists = false;
            rKey = NULL;
            return;
        }

        Key *startKey = NULL;
        if (NULL != currentRange->getStartKey())
        {
            startKey = new Key(currentRange->getStartKey ());

        }
        else {
            startKey = new Key ();
        }

        bool reseek = true;

        if (currentRange->getStopKey () != NULL && firstKey > currentRange->getStopKey ())
        {
            // range is before the first key;
            reseek = false;
        }

        if (rKey != NULL)
        {
            // already done work
        }
        else
        {

        }


        if (reseek)
        {
            iiter = index->lookup (startKey);

            close ();

            if (!iiter->isEnd ())
            {
                while (iiter->hasPrevious ())
                {	
		    IndexEntry *ent = *(*iiter);
		    if (*(iiter->getPrevious()) == *ent)
		      iiter->previous ();
		    else
		      break;
                }

                if (iiter->hasPrevious ())
                {
                    prevKey = new Key ((Key*) iiter->getPrevious ()->getKey ());
                }
                else
                    prevKey = new Key ();


                IndexEntry *indexEntry = iiter->get ();
                entriesLeft = indexEntry->getNumEntries ();

                if (version == 3 || version == 4)
                {
                    currentStream = getDataBlock (
                                        startBlock + iiter->getPreviousIndex ());
                }
                else
                {
                    currentStream = getDataBlock (
                                        startBlock + iiter->getPreviousIndex ());
                }
                checkRange = newSeekRequest->getRange ()->getStopKey ()
                             > indexEntry->getKey ();
                if (!checkRange)
                    topExists = true;
                delete indexEntry;

                // don't concern outselves with block indexing

                std::vector<char> valueArray;

                Key *currKey = 0;

                SkippedRelativeKey * skipRR = new SkippedRelativeKey (
                    currentStream, startKey, &valueArray, prevKey, currKey);

                if (skipRR->getPrevKey () != NULL)
                {
                    prevKey = new Key (skipRR->getPrevKey ());
                }
                else
                    prevKey = NULL;
                entriesLeft -= skipRR->getSkipped ();
                val = new Value ();
                val->setValue ((uint8_t*) valueArray.data (),
                               valueArray.size (), 0);
                rKey = skipRR->getRelativeKey ();
                delete skipRR;

            }
        }

        topExists = (rKey != NULL
                    && currentRange->getInfiniteStopKey()) || !(currentRange->getStopKey () > rKey->getStream ());
        while (hasTop () && !currentRange->getInfiniteStartKey() && currentRange->getStartKey () < getTopKey ())
        {
            next ();
        }
        delete startKey;
    }

    virtual void
    next ()
    {

        if (!hasTop ())
            throw std::runtime_error ("Illegal State Exception");
        if (entriesLeft == 0)
        {
            currentStream->close ();

            if (iiter->hasNext ())
            {
                iiter++;
                IndexEntry *indexEntry = iiter->get ();
                entriesLeft = indexEntry->getNumEntries ();
                if (version == 3 || version == 4)
                {
                    currentStream = getDataBlock (
                                        startBlock + iiter->getPreviousIndex ());
                }
                else
                {
                    currentStream = getDataBlock (
                                        startBlock + iiter->getPreviousIndex ());
                }
                checkRange = currentRange->getStopKey () > indexEntry->getKey ();
                if (!checkRange)
                    topExists = true;
            }

            else
            {
                rKey = 0;
                val = 0;
                topExists = false;
                return;
            }
        }
        prevKey = (Key*)rKey->getStream();
        rKey->read(currentStream);
        val->read(currentStream);
        entriesLeft--;

    }

    cclient::data::streams::InputStream *
    getDataBlock (uint32_t index)
    {


        BlockRegion *region = bcFile->getDataIndex ()->getBlockRegion (index);
        region->setCompressor(bcFile->getDataIndex ()->getCompressionAlgorithm ().create() );
        cclient::data::streams::InputStream *stream = region->readDataStream (reader);
        delete region;
        return stream;
    }

    cclient::data::streams::InputStream *
    getDataBlock (uint64_t offset, uint64_t compressedSize, uint64_t rawSize)
    {

//
        cclient::data::compression::Compressor *compressor =
            bcFile->getDataIndex ()->getCompressionAlgorithm ().create ();
        BlockRegion *region = new BlockRegion (offset, compressedSize, rawSize,
                                               compressor);
        cclient::data::streams::InputStream *stream = region->readDataStream (reader);
        delete region;
        return stream;
    }

}

;
}
}

#endif /* INCLUDE_DATA_CONSTRUCTS_RFILE_META_LOCALITYGROUPREADER_H_ */

