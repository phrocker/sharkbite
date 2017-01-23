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
#include "../../../../include/data/constructs/server/RangeDefinition.h"

#include <string>
#include <vector>



#include "../../../../include/data/constructs/server/ServerDefinition.h"
#include "../../../../include/data/constructs/server/../security/AuthInfo.h"
#include "../../../../include/data/constructs/server/../Range.h"
#include "../../../../include/data/constructs/server/../KeyExtent.h"
#include "../../../../include/data/constructs/column.h"

namespace cclient
{

namespace data
{

namespace tserver
{

/**
 * Constructor
 * @param creds ptr to credentials
 * @param auths pointer to auths used in range definition
 * @param host host name we're connecting to
 * @param port port connecting to
 * @param keyRange range
 * @param keyExt key extent
 * @param inCols incoming columns
 *
 */
RangeDefinition::RangeDefinition (cclient::data::security::AuthInfo *creds, cclient::data::security::Authorizations *auths,
                                  std::string host, uint32_t port,
                                  std::vector<Range*> *keyRange,
                                  std::vector<KeyExtent*> *keyExt,std::vector<Column*> *inCols) :
    ServerDefinition (creds, auths, host, port)
{
    if (NULL != keyRange)
    {

        ranges.insert (ranges.end (), keyRange->begin (), keyRange->end ());
    }

    if (NULL != keyExt)
        extents.insert (extents.end (), keyExt->begin (), keyExt->end ());
    
    if (NULL != inCols)
        columns.insert (columns.end (), inCols->begin (), inCols->end ());

}

/**
 * Returns ranges
 * @returns ranges
 */
std::vector<Range*> *
RangeDefinition::getRanges ()
{
    return &ranges;
}

/**
 * Returns key extents
 * @returns key extents;
 */
std::vector<KeyExtent*> *
RangeDefinition::getExtents ()
{
    return &extents;
}
}
}
}
