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
#ifndef BLOCKREGION_H_
#define BLOCKREGION_H_

#include "../../compressor/compressor.h"

#include "../../../streaming/Streams.h"
#include "../../../streaming/ByteOutputStream.h"
#include "../../../streaming/input/ByteInputStream.h"
#include "../../../streaming/input/NetworkOrderInputStream.h"

namespace cclient
{
namespace data
{


class BlockRegion : public cclient::data::streams::StreamInterface
{
public:
    BlockRegion () :
        offset (0), compressedSize (0), rawSize (0), compressor (NULL)
    {

    }

    BlockRegion (uint64_t offset, uint64_t compressedSize, uint64_t rawSize,
                 cclient::data::compression::Compressor *compressor) :
        offset (offset), compressedSize (compressedSize), rawSize (rawSize), compressor (
            compressor)
    {

    }

    BlockRegion (cclient::data::streams::InputStream *in)
    {
        read (in);
    }
    /**
     Copy constructor.
     @param object from which we copy our items.
     **/
    BlockRegion (const BlockRegion &copy)
    {

        *this = copy;
    }

    virtual ~BlockRegion() {

        delete compressor;
    }

    /**
     sets of the offset.
     @param off offset.
     **/
    void
    setOffset (uint32_t off)
    {
        offset = off;
    }

    /**
     Sets the compressed size
     @param csize compressed size.
     **/
    void
    setCompressedSize (uint32_t csize)
    {
        compressedSize = csize;
    }

    /**
     Sets the raw size
     **/
    void
    setRawSize (uint32_t rsize)
    {
        rawSize = rsize;
    }

    /**
     Sets the compressor
     @param comp compressor.
     **/
    void
    setCompressor (cclient::data::compression::Compressor *comp)
    {
        compressor = comp;
    }

    /**
     * Returns the reference to the compressor
     * Should not be constant as compressor could be used
     * and subsequently the internal components could be modified
     * @returns compressor reference
     */
    cclient::data::compression::Compressor *
    getCompressor ()
    {
        return compressor;
    }
    uint64_t
    read (cclient::data::streams::InputStream *in);

    uint64_t
    write (cclient::data::streams::OutputStream *out);

    cclient::data::streams::InputStream *
    readDataStream (cclient::data::streams::InputStream *in)
    {

        uint64_t pos = in->getPos();


        in->seek (offset);

        uint8_t *compressedValue = new uint8_t[compressedSize];

        in->readBytes (compressedValue, compressedSize);


        compressor->setInput ((const char*) compressedValue, 0, compressedSize);

        cclient::data::streams::ByteOutputStream *outStream = new cclient::data::streams::ByteOutputStream (rawSize);

        compressor->decompress (outStream);


        cclient::data::streams::EndianInputStream *returnStream = new cclient::data::streams::EndianInputStream (
            outStream->getByteArray (), outStream->getSize (), true);

        in->seek(pos);
        delete[] compressedValue;
        delete outStream;

        return returnStream;
    }

    BlockRegion &
    operator= (const BlockRegion &other)
    {

        offset = other.offset;
        compressedSize = other.compressedSize;
        rawSize = other.rawSize;
        compressor = other.compressor;
        return *this;
    }

    uint32_t
    getOffset ()
    {
        return offset;
    }

    uint32_t
    getCompressedSize ()
    {
        return compressedSize;
    }

    uint32_t
    getRawSize ()
    {
        return rawSize;
    }

protected:
    // compressor.
    cclient::data::compression::Compressor *compressor;
    // offset.
    uint32_t offset;
    // compressed size.
    uint32_t compressedSize;
    // raw size.
    uint32_t rawSize;
};
}
}
#endif /* BLOCKREGION_H_ */
