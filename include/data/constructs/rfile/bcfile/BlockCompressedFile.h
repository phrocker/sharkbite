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
#ifndef BLOCKCOMPRESSEDFILE_H_
#define BLOCKCOMPRESSEDFILE_H_

#include "data_index.h"
#include "meta_index.h"
#include "block_comp_stream.h"
#include "../../compressor/compressor.h"
#include "../../../streaming/Streams.h"
#include "../../../streaming/EndianTranslation.h"
#include "../../inputvalidation.h"
#include "../rfile_version.h"

// ... total of 16 bytes
static const uint8_t B_MAGIC_BCFILE[16] =
{   0xd1, 0x11, 0xd3, 0x68, 0x91, 0xb5, 0xd7, 0xb6, 0x39, 0xdf, 0x41, 0x40,
    0x92, 0xba, 0xe1, 0x50
};

namespace cclient
{
namespace data
{


class BlockCompressedFile : public cclient::data::streams::StreamInterface
{
public:
    /**
     * Constructor accepts a compressor as the only argument
     * @param compressor our compressor for this BCFile
     */
    explicit BlockCompressedFile (cclient::data::compression::Compressor *compressor) :
        compressorRef (compressor)
    {
        version.setMajor (1);
        version.setMinor (0);
        dataIndex.setCompressionAlgorithm (compressor);

    }

    /**
     * Constructor accepts a compressor as the only argument
     * @param compressor our compressor for this BCFile
     */
    BlockCompressedFile (cclient::data::streams::InputStream *in_stream, long fileLength) :
        in_stream (in_stream)
    {
        verifyStructure (fileLength);

    }

    void
    setDataIndex (DataIndex dataIndex)
    {
        this->dataIndex = dataIndex;
    }

    /**
     * Get the compressor. since it may be used
     * let's not set this function to constant
     * @returns compressor reference
     */
    cclient::data::compression::Compressor *
    getCompressor ()
    {
        return compressorRef;
    }

    DataIndex *
    getDataIndex ()
    {

        return &dataIndex;
    }

    MetaIndex *
    getMetaIndex ()
    {
        return &metaIndex;
    }

    cclient::data::streams::DataOutputStream *
    createCompressorStream (cclient::data::streams::OutputStream *out, MetaIndexEntry *entry)
    {
        return new BlockCompressorStream (out, compressorRef,
                                          entry->getRegion ());
    }

    cclient::data::streams::DataOutputStream *
    createDataStream (cclient::data::streams::OutputStream *out)
    {
        return new BlockCompressorStream (out, compressorRef,
                                          dataIndex.addBlockRegion ());
    }

    MetaIndexEntry *
    prepareNewEntry (std::string name)
    {
        return metaIndex.prepareNewEntry (name, compressorRef);
    }

    uint64_t
    write (cclient::data::streams::OutputStream *out)
    {

        out->writeBytes (B_MAGIC_BCFILE, 16);

        MetaIndexEntry *entry = metaIndex.prepareNewEntry ("BCFile.index",
                                compressorRef);

        BlockCompressorStream *blockStream = new BlockCompressorStream (
            out, compressorRef, entry->getRegion ());



        cclient::data::streams::BigEndianByteStream *byteStream =
        			dynamic_cast<cclient::data::streams::BigEndianByteStream*>(out);

        if (byteStream != nullptr){
        	std::cout << "byteStream is be" << std::endl;
        }
        else{
        	std::cout << "byteStream is le" << std::endl;
        }

        cclient::data::streams::ByteOutputStream *outStream = new cclient::data::streams::BigEndianByteStream(250 * 1024, blockStream);

        dataIndex.write (outStream);
        //dataIndex.write (blockStream);

        outStream->flush();
        blockStream->flush ();

        uint64_t offsetIndexMeta = out->getPos ();

        // should synchronize

        delete blockStream;

        delete outStream;

        metaIndex.write (out);
        out->writeLong (offsetIndexMeta);
        version.write (out);
        out->writeBytes (B_MAGIC_BCFILE, 16);
        //out->flush();
        return out->getPos ();
    }

    void
    close ()
    {

    }

protected:

    void
    verifyStructure (long fileLength)
    {
        const size_t magic_size = array_length (B_MAGIC_BCFILE);
        in_stream->seek (fileLength - magic_size - VERSION_SIZE);
        version.read (in_stream);
        uint8_t* magicVerify = new uint8_t[16];
        in_stream->readBytes (magicVerify, 16);
        if (memcmp (B_MAGIC_BCFILE, magicVerify, 16) != 0)
        {
            throw std::runtime_error ("Invalid Magic Number");
        }
        delete[] magicVerify;
        // get the index meta
        in_stream->seek (fileLength - magic_size - VERSION_SIZE - 8);
        offsetIndexMeta = in_stream->readLong ();


        in_stream->seek (offsetIndexMeta);

        metaIndex.read (in_stream);

        MetaIndexEntry *min = metaIndex.getEntry("BCFile.index");

        // should be using block comp stream?
        cclient::data::streams::InputStream *dataIndexStream = min->readDataStream(in_stream);

        dataIndex.read (dataIndexStream);
        delete dataIndexStream;
    }

   cclient::data::compression::Compressor *compressorRef;

    DataIndex dataIndex;
    MetaIndex metaIndex;
    RFileVersion version;

    // for reading
    cclient::data::streams::InputStream *in_stream;
    uint64_t offsetIndexMeta;
};

}
}
#endif /* BLOCKCOMPRESSEDFILE_H_ */

