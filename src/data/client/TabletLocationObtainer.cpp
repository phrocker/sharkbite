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

#include <map>
#include <algorithm>

#include "../../../include/data/client/TabletLocationObtainer.h"

#include "../../../include/data/client/../constructs/Key.h"
#include "../../../include/data/client/../constructs/value.h"
#include "../../../include/data/client/../streaming/input/NetworkOrderInputStream.h"

namespace cclient
{
namespace impl
{



TabletLocationObtainer::TabletLocationObtainer ()
{
    // TODO Auto-generated constructor stub

}

TabletLocationObtainer::~TabletLocationObtainer ()
{
    // TODO Auto-generated destructor stub
}

std::map<std::shared_ptr<cclient::data::Key> , std::shared_ptr<cclient::data::Value> , pointer_comparator<std::shared_ptr<cclient::data::Key> >>
        TabletLocationObtainer::decodeResults (std::vector<std::shared_ptr<cclient::data::KeyValue> > *results)
{

    std::map<std::shared_ptr<cclient::data::Key> , std::shared_ptr<cclient::data::Value> , pointer_comparator<std::shared_ptr<cclient::data::Key> >> sortedMap;

    for (std::vector<std::shared_ptr<cclient::data::KeyValue> >::iterator it = results->begin ();
            it != results->end (); it++)
    {

        std::shared_ptr<cclient::data::KeyValue> kv = (std::shared_ptr<cclient::data::KeyValue> ) (*it);

        std::shared_ptr<cclient::data::Key> key = kv->getKey ();
        std::shared_ptr<cclient::data::Value> value = kv->getValue ();
        std::pair<uint8_t*, size_t> pair = value->getValue ();

        cclient::data::streams::EndianInputStream *byteStream = new cclient::data::streams::EndianInputStream (
            (char*) pair.first, pair.second);

        int numKeys = byteStream->readInt ();
        for (int i = 0; i < numKeys; i++)
        {
            std::pair<uint32_t, uint8_t*> cf = readByteArray (byteStream);
            std::pair<uint32_t, uint8_t*> cq = readByteArray (byteStream);
            std::pair<uint32_t, uint8_t*> cv = readByteArray (byteStream);
            uint64_t timestamp = byteStream->readLong ();
            std::pair<uint32_t, uint8_t*> createdValue = readByteArray (
                        byteStream);

            std::shared_ptr<cclient::data::Key> newKey = std::make_shared<  cclient::data::Key> ();
            std::pair<const char*, uint32_t> row = key->getRow ();
            newKey->setRow (row.first, row.second);
            newKey->setColFamily ((const char*) cf.second, cf.first);
            newKey->setColQualifier ((const char*) cq.second, cq.first);
            newKey->setColVisibility ((const char*) cv.second, cv.first);
            newKey->setTimeStamp (timestamp);
            std::shared_ptr<cclient::data::Value> newValue = std::make_shared<  cclient::data::Value >();
            newValue->setValue (createdValue.second, createdValue.first);

            sortedMap.insert (std::make_pair (newKey, newValue));
            delete[] cf.second;
            delete[] cq.second;
	    delete[] cv.second;
            delete[] createdValue.second;
        }
        
        delete byteStream;

    }
    return sortedMap;
}

inline std::pair<uint32_t, uint8_t*>
TabletLocationObtainer::readByteArray (cclient::data::streams::InputStream *stream)
{
    int length = stream->readInt ();
    
    uint8_t * array = new uint8_t[length];

    stream->readBytes (array, length);

    return std::make_pair (length, array);
}

} /* namespace data */
} /* namespace cclient */
