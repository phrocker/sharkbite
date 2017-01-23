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
#ifndef INCLUDE_INTERCONNECT_STREAMING_DATASTREAM_H_
#define INCLUDE_INTERCONNECT_STREAMING_DATASTREAM_H_

#include <iterator>

#include "StreamEnvironment.h"
#include "StreamRelocation.h"

namespace cclient
{
namespace data
{
namespace streams
{

template<typename T>
class DataStream : public std::iterator<std::forward_iterator_tag, T>
{
protected:

    StreamConfiguration *myConfiguration;

public:

    DataStream ()
    {
    }

    void
    setConfiguration (StreamConfiguration *configuration)
    {
        myConfiguration = configuration;
    }
    virtual
    ~DataStream ()
    {
    }

    virtual bool
    hasNext () = 0;

    virtual void
    relocate (StreamRelocation *location) = 0;

    virtual DataStream
    *
    begin () = 0;

    virtual DataStream
    *
    end () = 0;

    virtual T
    operator* () = 0;

    virtual DataStream*
    operator++ () = 0;

    virtual void next() = 0;

    virtual DataStream*
    operator++ (int t) = 0;

};
}
}
}
#endif /* INCLUDE_INTERCONNECT_STREAMING_DATASTREAM_H_ */
