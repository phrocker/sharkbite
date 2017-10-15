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

#ifndef LOCALITYGROUPMETADATA_H_
#define LOCALITYGROUPMETADATA_H_


#include <map>

#include <vector>
#include <stdexcept>



#include "../../../streaming/ByteOutputStream.h"
#include "../../../streaming/NetworkOrderStream.h"
#include "../../../streaming/Streams.h"
#include "../../compressor/compressor.h"

#include "IndexManager.h"
#include "IndexEntry.h"

namespace cclient
{
namespace data
{

/**
 * Stores the locality group metadata
 */
class LocalityGroupMetaData : cclient::data::streams::StreamInterface
{

public:
    /**
     Constructor
     @param starBlockVal start block value
     @param name The name of the locality group.

     **/
    LocalityGroupMetaData (uint32_t startBlockVal, std::string name = "");

    LocalityGroupMetaData (cclient::data::compression::Compressor *compressorRef, int version,
                           cclient::data::streams::InputStream *reader);

    ~LocalityGroupMetaData ();

    /**
     Sets the first key
     @param key incoming key to set.
     **/
    void
    setFirstKey (std::shared_ptr<StreamInterface> key)
    {
        firstKey = key;
    }

    std::shared_ptr<StreamInterface> 
    getFirstKey ()
    {
        return firstKey;
    }

    uint32_t
    getStartBlock ()
    {
        return startBlock;
    }

    /**
     read function for the Locality Meta Data
     @param outStream output stream.
     @return position of output stream.
     **/
    uint64_t
    read (cclient::data::streams::InputStream *in);

    /**
     write function for the Locality Meta Data
     @param outStream output stream.
     @return position of output stream.
     **/
    uint64_t
    write (cclient::data::streams::DataOutputStream *outStream);

    /**
     Add the index entry to the meta data.
     @param ind incoming index entry.
     **/
    void
    addIndexEntry (IndexEntry ind)
    {
        index.push_back (std::move(ind));
    }

    LocalityGroupMetaData &
    operator= (const LocalityGroupMetaData &other)
    {
        startBlock = other.startBlock;
        firstKey = other.firstKey;
        offsets.insert (offsets.end (), other.offsets.begin (),
                        other.offsets.end ());

        columnFamilies = other.columnFamilies;

        return *this;
    }

    void
    setDefaultLG ()
    {
        isDefaultLG = true;

    }

    std::shared_ptr<IndexManager>
    getIndexManager ()
    {
        return indexManager;
    }

protected:

    /**
     Build the index array of IndexEntry objects.
     @return std pair containing the array and the size
     of this array. note that this array is allocated within
     this function, therefore, the memory must be freed
     in the calling function.
     **/
    std::pair<char*, size_t>
    buildIndexArray ()
    {
        cclient::data::streams::BigEndianByteStream *byteOutStream = new cclient::data::streams::BigEndianByteStream (
            index.size () * 120);
        cclient::data::streams::DataOutputStream *outputStream = new cclient::data::streams::DataOutputStream (byteOutStream);
        //BigEndianOutStream outputStream(&byteOutStream);

        uint64_t totalPos = 0;
        uint32_t off = byteOutStream->getPos ();
        for (std::vector<IndexEntry>::iterator it = index.begin (); it != index.end ();
                it++)
        {

            offsets.push_back (off);
            off = (*it).write (outputStream);
            totalPos += off;
        }

        char *arr = new char[off];

        memcpy (arr, byteOutStream->getByteArray (), off);
        delete byteOutStream;
        delete outputStream;
        return std::make_pair (arr, off);
    }
    // start block of this meta data group.
    uint32_t startBlock;
    // first key in the locality group.
    std::shared_ptr<StreamInterface> firstKey;
    // region of index entry offsets.
    std::vector<int> offsets;
    // column families for this locality group.
    std::map<std::pair<uint8_t *, size_t>, uint64_t> columnFamilies;
    // index entries.
    std::vector<IndexEntry> index;

    cclient::data::compression::Compressor *compressorRef;

    std::shared_ptr<IndexManager> indexManager;
    // boolean value identifying this as the default locality group.
    bool isDefaultLG;
    // name of this locality group.
    std::string name;
};
}
}
#endif /* LOCALITYGROUPMETADATA_H_ */
