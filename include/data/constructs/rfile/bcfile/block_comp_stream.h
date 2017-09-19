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
#ifndef BLOCK_COMP_STREAM_H_
#define BLOCK_COMP_STREAM_H_

#include <vector>


#include "../../compressor/compressor.h"
#include "../../../streaming/Streams.h"
#include "../../../streaming/input/NetworkOrderInputStream.h"
#include "BlockRegion.h"

namespace cclient {
namespace data {

class BlockStreambuffer: public std::streambuf {
public:

    explicit BlockStreambuffer(size_t s) :
        bfptr(new char[s]), max_size(s) {

    }
    virtual ~BlockStreambuffer() {
        delete[] bfptr;
    }

    char *getPtr() {

        return bfptr;
    }

    virtual std::streamsize xsputn(const char * s, std::streamsize n) = 0;

protected:
    size_t max_size;
private:
    char* bfptr;
};

class BlockCompressorStream: private BlockStreambuffer,
    public cclient::data::streams::DataOutputStream,
    public cclient::data::streams::EndianInputStream,
    public std::ostream,
    public std::istream {

public:
    // takes ownership of the compressor
    BlockCompressorStream(OutputStream *out_stream, cclient::data::compression::Compressor *compressor,
                          BlockRegion *region);

    BlockCompressorStream(InputStream *in_stream,cclient::data::compression::Compressor *decompressor,BlockRegion *region);

    ~BlockCompressorStream();

    inline virtual int sync() {
	return 0;

    }
    /**
     * Returns the number of bytes written
     * @returns number of bytes written by the compressor
     * and any remaining data left in the growing buffer.
     **/
    uint32_t bytesWritten() {
        return compress->bytesWritten() + growingBuffer.size();
    }

    /**
     * Returns the current position.
     * @returns the current position of the output stream.
     */
    uint64_t getPos() {
        if (NULL != output_stream) {
            return output_stream->getPos();
        } else {
            return EndianInputStream::getPos();
        }

    }

    /**
     * Returns the reference to the compressor
     */
    cclient::data::compression::Compressor *getCompressor() {

        return compress;
    }

    /**
     * Flushes the block compressor stream.
     * should be called when finished or by xsputn
     */
    void flush() {

        output_stream_ref->getPos();
        ostream_ref->flush();
        size_t location = growingBuffer.size();
        if (location == 0)
            return;

        // copy the input bufer to the compressor and call the
        // compress function on it.

        compress->setInput(&growingBuffer.at(0), 0, location);
        compress->compress(output_stream);

        associatedRegion->setOffset(compress->getStreamOffset());
        associatedRegion->setRawSize(location);
        associatedRegion->setCompressedSize(compress->getCompressedSize());
        // clear the buffer and the block location so that
        // this object can be reused.
        growingBuffer.clear();
        blockLoc = 0;
    }

    inline int overflow( int ch ) {
      return ch;
    }

    /**
     * Function is automatically called by the output stream since
     * we are based on sstream.
     * @param s the input buffer
     * @param n stream size
     * @returns stream size written
     */

    inline std::streamsize xsputn(const char * s, std::streamsize n) {

        if (n == 0)
            return 0;
        // if we have not started writing we need to set the stream
        // offset of the compressor
        if (!writeStart)
            compress->setStreamOffset(output_stream->getPos());

        writeStart = true;

        size_t location = growingBuffer.size();
        // resize the growing buffer.
        growingBuffer.resize(location + n);

        memcpy(&growingBuffer.at(location), s, n);

        return n;
    }

protected:
    // output steram.
    OutputStream *output_stream;
    // block location.
    uint64_t blockLoc;
    std::vector<char> growingBuffer;
    // identifies whether compression has started
    // if not true, then we neet to set the stream offset
    bool writeStart;
    // compressor reference.
    cclient::data::compression::Compressor  *compress;
    BlockRegion *associatedRegion;

    InputStream *input_stream;

};
}
}
#endif /* BLOCK_COMP_STREAM_H_ */
