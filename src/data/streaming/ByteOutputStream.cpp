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
#include <string.h> /* memset */
#include <vector>
#include <assert.h>
#include <stdlib.h>
#include "../../../include/data/streaming/ByteOutputStream.h"

namespace cclient{
  namespace data{
    namespace streams{

/**
 * Byte output stream constructor
 * @param initial_size initial size of byte array
 * @param out_stream output stream to which we're directing our data
 */
ByteOutputStream::ByteOutputStream (size_t initial_size,
                                    OutputStream *out_stream) :
    offset (0), output_stream_ref (out_stream)
{
    size = initial_size;
    array.resize(size);
    std::fill(array.begin(), array.end(), 0x00);
}

/**
 * Destructor. flushes the array then removes it
 */
ByteOutputStream::~ByteOutputStream ()
{
    flush ();
}

/**
 * Flush writes the array to the out_stream defined in the constructor
 */
void
ByteOutputStream::flush ()
{
    if (output_stream_ref != NULL)
    {
        output_stream_ref->write (array.data(), offset);
    }
    offset = 0;
}

/**
 * Returns the current offset
 * @return current offset
 */
uint64_t
ByteOutputStream::getPos ()
{
    return offset;
}

/**
 * Copies byte array into arguments
 * @param inArray incoming array
 * @param size of incoming array
 * @throws runtime_error if input size isn't enough to hold the current data.
 */
void
ByteOutputStream::getByteArray (char *inArray, size_t inArraySize)
{
    if (inArraySize < offset)
    {
        throw std::runtime_error ("Sizes are unequal");
    }
    //memcpy (inArray, array, offset);
    std::copy(array.data(),array.data()+offset,inArray);
    //memcpy (inArray, array, offset);
}

/**
 * Returns the backing array
 * @returns backing array
 */
char *
ByteOutputStream::getByteArray ()
{
    return array.data();
}

/**
 * Returns the current size of the array. may differ from offset
 * @returns current size.
 */
size_t
ByteOutputStream::getSize ()
{
    return size;
}

/**
 * Allows override of output stream
 * @param out_stream new output stream
 */
void
ByteOutputStream::setOutputStreamRef (OutputStream *out_stream)
{
    output_stream_ref = out_stream;
}

/**
 * Writes generic byte array to internal array
 * @param bytes incomign byte array to write
 * @param cnt size of array
 */
uint64_t
ByteOutputStream::write (const char *bytes, long cnt)
{

    if (size - offset < (uint64_t)cnt)
    {
        // we don't have space, so create
        // a new array that we can copy to
        //char *nArray = new char[size + (cnt * 2)];
	 array.resize(size+(cnt*2));
       // memcpy (nArray, array, offset);
	//std::copy(array,array+offset,nArray);

        //elete[] array;
        //array = nArray;
        size += cnt * 2;
    }
    //memcpy (array + offset, bytes, cnt);
    std::copy(bytes,bytes+cnt,array.data()+offset);
    offset += cnt;
    return offset;
}

/**
 * writes a single byte
 * @param byte incoming byte to write.
 * @returns offset
 */
uint64_t
ByteOutputStream::writeByte (int byte)
{
    uint8_t bt = 0xFF & byte;
    return writeByte (bt);
}

/**
 * Writes the std::string to the byte array
 * @param std::string to write
 * @returns offset
 */
uint64_t
ByteOutputStream::writeString (std::string s)
{
    // write size of string
    writeHadoopLong (s.size ());
    return writeBytes ((uint8_t*) s.data (), s.size ());
}

/**
 * Writes character array
 * @param bytes incoming bytes
 * @param cnt counter
 * @offset
 */
uint64_t
ByteOutputStream::write (const uint8_t *bytes, long cnt)
{
    return write ((const char*) bytes, cnt);
}

/**
 * Writes character array
 * @param bytes incoming bytes
 * @param cnt counter
 * @offset
 */
uint64_t
ByteOutputStream::writeBytes (const uint8_t *bytes, size_t cnt)
{
    return write ((const char*) bytes, cnt);
}

/**
 * Writes byte
 * @param byte byte to write
 * @offset
 */
uint64_t
ByteOutputStream::writeByte (const uint8_t byte)
{
    return write ((const char*) &byte, 1);

}

/**
 * Writes short
 * @param shortVal short to write
 * @offset
 */
uint64_t
ByteOutputStream::writeShort (const short shortVal)
{

    return write ((const char*) &shortVal, 2);
}

/**
 * Writes int
 * @param intVal short to write
 * @offset
 */
uint64_t
ByteOutputStream::writeInt (const int intVal)
{
    return write ((const char*) &intVal, 4);

}

/**
 * Writes long
 * @param val long value to write
 * @offset
 */
uint64_t
ByteOutputStream::writeLong (const uint64_t val)
{
    return write ((const char*) &val, 8);

}

/**
 * Writes a boolean value
 * @param val long value to write
 * @offset
 */
uint64_t
ByteOutputStream::writeBoolean (const bool val)
{
    uint8_t byte = 0x00;
    if (val)
        byte = 0x01;
    return write ((const char*) &byte, 1);
}

    }
  }
}