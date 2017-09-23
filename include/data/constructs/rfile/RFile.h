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

#ifndef RFILE_H_
#define RFILE_H_

#include <vector>
#include <iterator>


#include "../compressor/compressor.h"
#include "../../streaming/Streams.h"
#include "../../streaming/accumulo/KeyValueIterator.h"
#include "../../streaming/input/InputStream.h"
#include "../KeyValue.h"

// meta
#include "meta/MetaBlock.h"
#include "meta/LocalityGroupReader.h"

// bcfile
#include "bcfile/meta_index.h"
#include "bcfile/data_index.h"
#include "bcfile/BlockCompressedFile.h"

namespace cclient
{
namespace data
{

class RFile : public cclient::data::streams::StreamInterface, public cclient::data::streams::KeyValueIterator
{

public:
    /**
     Constructor
     @param output_stream output stream for the r file
     @param bWriter block compressed writer.
     @param blockSize the desired block size of the bc file.
     **/
    RFile (cclient::data::streams::OutputStream *output_stream, BlockCompressedFile *bWriter);

    /**
     Constructor
     @param input stream output stream for the r file
     @param bWriter block compressed writer.
     @param blockSize the desired block size of the bc file.
     **/
    RFile (cclient::data::streams::InputStream *input_stream, long fileLength);

    virtual
    ~RFile ();

    /**
     Get the maximum block size.
     @return block size.
     */
    uint32_t
    getMaxBlockSize ()
    {
        return maxBlockSize;
    }

    /**
     Gets the pointer to the number of entries.
     @Deprecated. this isn't needed!
     @return pointer to entries.
     **/
    uint32_t *
    getEntryPtr ()
    {
        return &entries;
    }

    /**
     Creates a data stream from the bc file strema.
     @return pointer to new stream.
     **/
    BlockCompressorStream *
    createStream ()
    {
        return (BlockCompressorStream*) blockWriter->createDataStream (
                   myDataStream);
    }

    /**
     Sets the first key for the current locality group.
     @param key incoming key to set.

     */
    void
    setCurrentLocalityKey (StreamInterface *key)
    {
        if (key == NULL)
        {
            throw std::runtime_error ("Key should not be null.");
        }
        currentLocalityGroup->setFirstKey (key);
    }

    static uint32_t
    generate_average (std::vector<StreamInterface*> *keyValues)
    {
        cclient::data::streams::ByteOutputStream outStream (1024 * 1024);
        for (int i = 0; i < 10; i++)
        {
            keyValues->at (i)->write (&outStream);
        }

        uint32_t average = outStream.getPos () / 10;

        return average;

    }

    bool
    append (std::vector<StreamInterface*> *keyValues, bool isSorted = false)
    {
        return append (keyValues, generate_average (keyValues), isSorted);
    }

    bool
    append (KeyValue *kv);

    bool
    append (std::vector<StreamInterface*> *keyValues, uint32_t average_recordSize,
            bool isSorted);

    /**
     Add a locality group
     @param name name of locality group. If it is empty, then we have a default
     locality group.
     **/
    void
    addLocalityGroup (std::string name = "")
    {
        closeCurrentGroup ();
        LocalityGroupMetaData *group = new LocalityGroupMetaData (dataBlockCnt,
                name);
        currentLocalityGroup = group;
        dataBlockCnt++;

    }
    /**
     Closes the RFile.
     **/
    void
    close ();
    /**
     Closes the data section of the RFile.
     **/
    void
    closeData ()
    {
        if (dataClosed)
            return;

        if (currentBlockWriter != NULL)
        {
            currentBlockWriter->flush ();
            closeBlock (lastKeyValue->getKey ());
            //      currentBlockWriter->close();
	    delete currentBlockWriter;
            currentBlockWriter = NULL;
        }

        dataClosed = true;

    }
    /**
     Closes a block, setting the last key before closing it
     and adding that entry to the current locality group.
     @param lastKey last key for locality group.
     **/
    void
    closeBlock (StreamInterface *lastKey)
    {
    	std::cout << "Close block " << entries << lastKey << std::endl;
        currentLocalityGroup->addIndexEntry (IndexEntry(lastKey, entries));
        dataBlockCnt = 0;
        entries = 0;
        currentBlockCount=0;

    }

    virtual bool
    hasNext ()
    {
        return currentLocalityGroupReader->hasTop();
    }

    virtual void
    relocate (cclient::data::streams::StreamRelocation *location)
    {
        currentLocalityGroupReader->seek(location);
    }


    virtual void next()
    {

        currentLocalityGroupReader->next();
    }

    virtual DataStream*
    operator++ ()
    {

        currentLocalityGroupReader->next();
        return this;
    }


    friend inline std::ostream &
    operator << (std::ostream &out, RFile &rhs)
    {
        std::map<std::string,MetaIndexEntry> *entries = rhs.blockWriter->getMetaIndex()->getEntries();

        for (std::map<std::string, MetaIndexEntry>::iterator it = entries->begin ();
                it != entries->end (); it++)
        {
            out << "Meta Block	: " << (*it).first << std::endl;
            MetaIndexEntry *entry = &((*it).second);
            out << "	Raw size	:" << entry->getRegion()->getRawSize() << std::endl;
            out << "	Compressed size	:" << entry->getRegion()->getCompressedSize() << std::endl;
        }

        return out;
    }

    friend inline std::ostream &
    operator << (std::ostream &out, RFile *rhs)
    {
        return operator<< (out, *rhs);
    }

    virtual std::pair<Key*, Value*>
    operator* ()
    {

        return std::make_pair (currentLocalityGroupReader->getTopKey(), (Value*) NULL);
    }

protected:

    void
    readLocalityGroups (cclient::data::streams::InputStream *metaBlock);

    /**
     Closes the current locality group.
     **/
    void
    closeCurrentGroup ()
    {
        if (currentLocalityGroup != NULL)
        {
            localityGroups.push_back (currentLocalityGroup);
            currentLocalityGroup = NULL;
            dataBlockCnt = 0;
            entries = 0;
        }

    }



    // current locality group.
    LocalityGroupMetaData *currentLocalityGroup;
    LocalityGroupReader *currentLocalityGroupReader;
    // number of entries in current block.
    uint32_t entries;
    uint32_t currentBlockCount;
    KeyValue *lastKeyValue;
    // current data block count in locality group.
    uint16_t dataBlockCnt;
    // output stream
    cclient::data::streams::OutputStream *out_stream;
    // data output stream.
    cclient::data::streams::OutputStream *myDataStream;

    cclient::data::streams::InputStream *myInputStream;

    // list of locality group pointers.
    std::vector<LocalityGroupMetaData*> localityGroups;
    std::vector<LocalityGroupReader*> localityGroupReaders;

    // block compressed file.
    BlockCompressedFile *blockWriter;
    // compressor reference.
    cclient::data::compression::Compressor *compressorRef;
    // current block writer, created from blockWriter.
    BlockCompressorStream *currentBlockWriter;
    // maximum block size.
    uint32_t maxBlockSize;
    // boolean identifying a closed data block.
    bool dataClosed;
    // boolean identifying closed rfile.
    bool closed;

    // primarily for reading
    cclient::data::streams::InputStream *in_stream;

};
}
}
#endif /* RFILE_H_ */
