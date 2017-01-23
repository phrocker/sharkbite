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
#ifndef INCLUDE_DATA_STREAMING_ACCUMULO_STREAMSEEKABLE_H_
#define INCLUDE_DATA_STREAMING_ACCUMULO_STREAMSEEKABLE_H_

#include <cstdlib>

#include <vector>
#include "../StreamRelocation.h"
#include "../../constructs/Range.h"

namespace cclient
{
namespace data
{
namespace streams
{


class StreamSeekable : public StreamRelocation
{
protected:
    Range *range;
    std::vector<uint8_t*> columnFamilies;
    bool inclusive;
public:

    StreamSeekable (Range *range, std::vector<uint8_t*> columnFamilies,
                    bool inclusive) :
        range (range), columnFamilies (columnFamilies), inclusive (
            inclusive)
    {

    }

    Range *
    getRange ()
    {
        return range;
    }

    std::vector<uint8_t*> *
    getColumnFamilies ()
    {
        return &columnFamilies;
    }

    bool
    isInclusive ()
    {
        return inclusive;
    }
};

}
}
}

#endif /* INCLUDE_DATA_STREAMING_ACCUMULO_STREAMSEEKABLE_H_ */
