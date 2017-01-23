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

#ifndef SCANBATCH_H_
#define SCANBATCH_H_

#include "KeyExtent.h"
#include "inputvalidation.h"
#include "../exceptions/IllegalArgumentException.h"
namespace cclient
{
namespace data
{

class ScanBatch
{
public:
    explicit ScanBatch(KeyExtent *extent) :
        myExtent(extent)
    {
        if (IsEmpty(extent))
        {
            throw cclient::exceptions::IllegalArgumentException("Extent is not allowed to be null");
        }
    }

    void addRange(Range *range)
    {
        ranges.insert(range);
    }


    virtual ~ScanBatch()
    {

    }
protected:
    KeyExtent *myExtent;
    std::set<Range*> ranges;
};

} /* namespace data */
} /* namespace cclient */
#endif /* SCANBATCH_H_ */
