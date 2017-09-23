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
#include "data/streaming/OutputStream.h"
#include <iostream>

namespace cclient{
  namespace data{
    namespace streams{
OutputStream::OutputStream (std::ostream *ptr, uint64_t pos) :
    ostream_ref (ptr), position (new uint64_t (pos)), copy (false)
{
}

OutputStream::OutputStream () :
    ostream_ref (NULL), position (new uint64_t (0)), copy (false)
{

}

OutputStream::~OutputStream ()
{
    flush ();
    if (!copy)
        delete position;
}

void
OutputStream::flush ()
{
    if (ostream_ref != NULL)
        ostream_ref->flush ();
}

uint64_t
OutputStream::getPos ()
{
    return *position;
}

uint64_t
OutputStream::write (const char *bytes, long cnt)
{

    ostream_ref->write ((const char*) bytes, cnt);
    *position += cnt;
    return *position;
}

uint64_t
OutputStream::writeByte (int byte)
{
    uint8_t bt = 0xFF & byte;
    return writeByte (bt);
}

uint64_t
OutputStream::writeString (std::string s)
{
    // write size of string
	std::cout << "write encoded long " << s.size() << " at " << getPos() << std::endl;
    writeEncodedLong (s.size ());
    return writeBytes ((uint8_t*) s.data (), s.size ());
}

uint64_t
OutputStream::write (const uint8_t *bytes, long cnt)
{
    ostream_ref->write ((const char*) bytes, cnt);
    *position += cnt;
    return *position;
}

uint64_t
OutputStream::writeBytes (const uint8_t *bytes, size_t cnt)
{
    ostream_ref->write ((const char*) bytes, cnt);
    *position += cnt;
    return *position;
}

uint64_t
OutputStream::writeBytes (const char *bytes, size_t cnt)
{
    return writeBytes ((const uint8_t*) bytes, cnt);
}

uint64_t
OutputStream::writeByte (const uint8_t byte)
{
    ostream_ref->write ((const char*) &byte, 1);
    *position += 1;
    return *position;
}

uint64_t
OutputStream::writeShort (const short shortVal)
{
    //  memcpy(shortByte,shortVal,2);
    ostream_ref->write ((const char*) &shortVal, 2);
    *position += 2;
    return *position;
}

uint64_t
OutputStream::writeInt (const int intVal)
{
    //  memcpy(intByte,intVal,2);
    ostream_ref->write ((const char*) &intVal, 4);
    *position += 4;
    return *position;
}

uint64_t
OutputStream::writeLong (const uint64_t val)
{
    ostream_ref->write ((const char*) &val, 8);
    *position += 8;
    return *position;
}

uint64_t
OutputStream::writeBoolean (const bool val)
{
    uint8_t byte = 0x00;
    if (val)
        byte = 0x01;
    ostream_ref->write ((const char*) &byte, 1);
    *position += 1;
    return *position;
}

uint64_t
OutputStream::writeVLong (const int64_t n)
{
    int64_t i = n;
    if (i >= -112L && i <= 127L)
    {
        write ((const char*) &i, 1);
    }
    else
    {
        int len = -112;
        if (i < 0L)
        {
            i = ~i;
            len = -120;
        }

        for (long tmp = i; tmp != 0L; --len)
        {
            tmp >>= 8;
        }
        char b = (char) len;
        write ((const char*) &b, 1);
        len = len < -120 ? -(len + 120) : -(len + 112);

        for (int idx = len; idx != 0; --idx)
        {
            int shiftbits = (idx - 1) * 8;
            long mask = 255L << shiftbits;
            char val = (char) ((int) ((i & mask) >> shiftbits));
            write ((const char*) &val, 1);
            //stream.writeByte((byte)((int)((i & mask) >> shiftbits)));
        }

    }

    return getPos ();

}

uint64_t
OutputStream::writeEncodedLong (const int64_t n)
{
	std::cout << "write encoded long " << n << std::endl;
    if ((n < 128) && (n >= -32))
    {

        write ((const char*) &n, 1);
        return *position;
        //return writeByte((int) n);

    }

    long un = (n < 0) ? ~n : n;
    int temp = n;
    // how many bytes do we need to represent the number with sign bit?
    int len = (64 - numberOfLeadingZeros (un)) / 8 + 1;
    int firstByte = (int) (n >> ((len - 1) * 8));
    switch (len)
    {
    case 1:
        // fall it through to firstByte==-1, len=2.
        firstByte >>= 8;
    case 2:
        if ((firstByte < 20) && (firstByte >= -20))
        {
            temp =firstByte - 52;
            write ((const char*) &temp, 1);
            write ((const char*) &n, 1);
            return *position;
            //writeByte(firstByte - 52);
            //return writeByte((int) n);
        }
        // fall it through to firstByte==0/-1, len=3.
        firstByte >>= 8;
    case 3:
        if ((firstByte < 16) && (firstByte >= -16))
        {
            temp = firstByte - 88;
            write ((const char*) &temp, 1);
            write ((const char*) ((short*) &n), 2);
            //writeByte(firstByte - 88);
            //return writeShort((int) n);
        }
        // fall it through to firstByte==0/-1, len=4.
        firstByte >>= 8;
    case 4:
        if ((firstByte < 8) && (firstByte >= -8))
        {
            /*
             writeByte(firstByte - 112);
             writeShort(((int) n) >> 8);
             return writeByte((int) n);
             */
            temp = firstByte - 112;
            write ((const char*) &temp, 1);
            temp = n >> 8;
            write ((const char*) ((short*) &temp), 2);
            //ostream_ref->write((const char*)((short*)&n),2);
            write ((const char*) &n, 1);
            return *position;
        }
        temp = len - 129;
        write ((const char*) &temp, 1);
        write ((const char*) &n, 4);
        return *position;
    //writeByte(len - 129);

    ///return writeInt((int) n);
    case 5:
        temp = len - 129;
        write ((const char*) &temp, 1);
        temp = n >> 8;
        write ((const char*) &temp, 4);
        write ((const char*) &n, 1);
        return *position;
    /*
     writeByte(len - 129);
     writeInt((int) (n >> 8));
     return writeByte((int) n);
     */
    case 6:
        temp = len - 129;
        write ((const char*) &temp, 1);
        temp = n >> 16;
        write ((const char*) &temp, 4);
        write ((const char*) ((short*) &n), 2);
        return *position;
    /*
     writeByte(len - 129);
     writeInt((int) (n >> 16));
     return writeShort((int) n);
     */
    case 7:
        temp = len - 129;
        write ((const char*) &temp, 1);
        temp = n >> 24;
        write ((const char*) &temp, 4);
        temp = n >> 8;
        write ((const char*) ((short*) &temp), 2);
        write ((const char*) ((short*) &n), 1);
        return *position;
    /*
     writeByte(len - 129);
     writeInt((int) (n >> 24));
     writeShort((int) (n >> 8));
     return writeByte((int) n);
     */
    case 8:
        temp = len - 129;
        write ((const char*) &temp, 1);

        write ((const char*) &n, 8);
        return *position;
    /*
     writeByte(len - 129);
     return writeLong(n);
     */
    default:
        return *position;
    };
    return *position;
}

uint64_t
OutputStream::writeHadoopLong (const int64_t n)
{
    int64_t i = n;
    if (i >= -112L && i <= 127L)
    {
        write ((const char*) &i, 1);
    }
    else
    {
        int len = -112;
        if (i < 0L)
        {
            i = ~i;
            len = -120;
        }

        for (long tmp = i; tmp != 0L; --len)
        {
            tmp >>= 8;
        }
        char b = (char) len;
        write ((const char*) &b, 1);
        len = len < -120 ? -(len + 120) : -(len + 112);

        for (int idx = len; idx != 0; --idx)
        {
            int shiftbits = (idx - 1) * 8;
            long mask = 255L << shiftbits;
            char val = (char) ((int) ((i & mask) >> shiftbits));
            write ((const char*) &val, 1);
        }

    }

    return getPos ();

}

uint32_t
OutputStream::bytesWritten ()
{

    return *position;
}
    }
  }
}
