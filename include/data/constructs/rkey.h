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

#ifndef R_KEY
#define R_KEY

#include "Key.h"
#include <stdint.h>

#include "../streaming/Streams.h"

#include <stdint.h>
#include <cstdio>
#include <vector>
#include <cstring>
#include <stdexcept>

namespace cclient
{
namespace data
{


/**
 * Based on the accumulo relative key.
 */
class RelativeKey : public cclient::data::streams::StreamInterface
{

public:
    /**
     * Constructor
     **/
    RelativeKey ();

    /**
     * Constructor.
     * @param previous_key previous key 
     * @param my_key current key.
     **/
    RelativeKey (const std::shared_ptr<Key> &previous_key,const std::shared_ptr<Key> &my_key);

    /**
     * Returns the relative key.
     **/
    std::shared_ptr<StreamInterface> 
    getStream ();

    /**
     * Sets the base for the relative key
     * @param my_key key to set on the relative key.
     **/
    void
    setBase (const std::shared_ptr<Key> &my_key);
    

    /**
     * create and fulfill the relative key object.
     **/
    virtual uint64_t read (cclient::data::streams::InputStream *in);

    /**
     * Sets the previous key for this relative key.
     * @param previous_key prev key.
     **/
    void
    setPrevious (const std::shared_ptr<Key> &previous_key);

    /**
     * write function for relative key
     * @param outStream output stream.
     **/
    uint64_t
    write (cclient::data::streams::OutputStream *outStream);

    bool
    operator < (const RelativeKey &rhs) const;

    bool
    operator < (const RelativeKey *rhs) const;

    ~RelativeKey ();

    // flags taken from java relative key
    static const uint8_t ROW_SAME = 0x01;
    static const uint8_t CF_SAME = 0x02;
    static const uint8_t CQ_SAME = 0x04;
    static const uint8_t CV_SAME = 0x08;
    static const uint8_t TS_SAME = 0x10;
    static const uint8_t DELETED = 0x20;

    static const uint8_t ROW_PREFIX = 0x01;
    static const uint8_t CF_PREFIX = 0x02;
    static const uint8_t CQ_PREFIX = 0x04;
    static const uint8_t CV_PREFIX = 0x08;
    static const uint8_t TS_DIFF = 0x10;

    static const uint8_t PREFIX_COMPRESSION_ENABLED = 128;

    const std::shared_ptr<Key> &getKey(){
    	return key;
    }
protected:

    void readPrefix (cclient::data::streams::InputStream *stream, std::pair<char*,size_t> *row, std::pair<char*,size_t> *prevRow);
    void readPrefix (cclient::data::streams::InputStream *stream, std::vector<char> *row, std::vector<char> *prevRow);


    inline bool readPrefix (cclient::data::streams::InputStream *stream, int *comparison, uint8_t SAME_FIELD,
                uint8_t PREFIX, char fieldsSame,char fieldsPrefixed, std::pair<char*,size_t> *field,
                std::pair<char*,size_t> *prevField)
    {

        if ((fieldsSame & SAME_FIELD) != SAME_FIELD)
        {
            if ((fieldsPrefixed & PREFIX) == PREFIX)
            {
                readPrefix (stream, field, prevField);
            }
            else
            {
                read (stream, field);
            }
            return true;
        }
        else {
            //field->resize(prevField->size());
            field->second = prevField->second;
            field->first = new char[ prevField->second+1 ];
            memcpy(field->first,prevField->first,prevField->second);
            
            //field->insert(field->begin(),prevField->data (),prevField->data ()+prevField->size());
        }
        return false;
    }

    inline bool readPrefix (cclient::data::streams::InputStream *stream, int *comparison, uint8_t SAME_FIELD,
                uint8_t PREFIX, char fieldsSame,char fieldsPrefixed, std::vector<char> *field,
                std::vector<char> *prevField)
    {

        if ((fieldsSame & SAME_FIELD) != SAME_FIELD)
        {
            if ((fieldsPrefixed & PREFIX) == PREFIX)
            {
                readPrefix (stream, field, prevField);
            }
            else
            {
                read (stream, field);
            }
            return true;
        }
        else {
            //field->resize(prevField->size());
            field->insert(field->begin(),prevField->data (),prevField->data ()+prevField->size());
        }
        return false;
    }


    inline void read (cclient::data::streams::InputStream *stream, std::vector<char> *row)
    {
        uint32_t len = stream->readEncodedLong();
        read (stream, row, len);
    }

    inline void read (cclient::data::streams::InputStream *stream, std::vector<char> *input, uint32_t len)
    {
        char *array = new char[len];
        stream->readBytes (array, len);

        input->insert (input->begin (), array, array + len);
        delete[] array;
    }


    inline void read (cclient::data::streams::InputStream *stream, std::pair<char*,size_t> *row)
    {
        uint32_t len = stream->readEncodedLong();
        read (stream, row, len);
    }

    inline void read (cclient::data::streams::InputStream *stream, std::pair<char*,size_t> *input, uint32_t len)
    {
        //char *array = new char[len];'
        input->first = new char [ len+1 ];
        input->second = len;
        stream->readBytes (input->first, len);

        //input->insert (input->begin (), array, array + len);
        //delete[] array;
    }

    inline int
    commonPrefix (std::pair<char*, size_t> prev,
                  std::pair<char*, size_t> curr);

    inline void
    writePrefix (cclient::data::streams::OutputStream *outStream, std::pair<char*, size_t> *var,
                 int commonPrefixLength)
    {
        // should be writevint, below
        outStream->writeVLong (commonPrefixLength);
        uint32_t remainder = var->second - commonPrefixLength;
        outStream->writeVLong (remainder);
        outStream->writeBytes (var->first + commonPrefixLength, remainder);

    }

    std::shared_ptr<Key> key;

    char fieldsSame;

    std::shared_ptr<Key> prevKey;

    void
    setKey (const std::shared_ptr<Key> &keyToCopy, const std::shared_ptr<Key> &keyToCopyTo);

    inline bool
    isSame (std::pair<char*, size_t> a, std::pair<char*, size_t> b);

    int32_t rowCommonPrefixLen;
    int32_t cfCommonPrefixLen;
    int32_t cqCommonPrefixLen;
    int32_t cvCommonPrefixLen;
    uint8_t fieldsPrefixed;
    long tsDiff;

};
}
}

#endif

