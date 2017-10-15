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
#ifndef INCLUDE_INTERCONNECT_ACCUMULO_KEYVALUEITERATOR_H_
#define INCLUDE_INTERCONNECT_ACCUMULO_KEYVALUEITERATOR_H_

#include "../../constructs/Key.h"
#include "../../constructs/value.h"
#include "../DataStream.h"

namespace cclient
{
namespace data
{
namespace streams
{


class KeyValueIterator : public DataStream<std::pair<std::shared_ptr<Key>, std::shared_ptr<Value>>>
{
public:
    KeyValueIterator (KeyValueIterator *source,
                      StreamConfiguration *configuration)
    {
        setConfiguration (configuration);
    }

    KeyValueIterator ()
    {

    }

    virtual
    ~KeyValueIterator ()
    {
    }

    virtual bool
    hasNext ()
    {
        return false;
    }

    virtual void
    relocate (StreamRelocation *location)
    {
    }

    virtual DataStream *
    begin ()
    {
        return this;
    }

    virtual DataStream *
    end ()
    {
        return this;
    }

    virtual std::pair<std::shared_ptr<Key>, std::shared_ptr<Value>>
    operator* ()
    {
        return std::make_pair (nullptr,nullptr);
    }

    virtual void next()
    {

    }

    virtual DataStream*
    operator++ ()
    {
        return this;
    }

    virtual DataStream*
    operator++ (int t)
    {
        return this;
    }
}
;
}
}
}
#endif /* INCLUDE_INTERCONNECT_ACCUMULO_KEYVALUEITERATOR_H_ */
