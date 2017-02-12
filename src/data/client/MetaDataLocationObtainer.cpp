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
#include <vector>
#include <memory>


#include "../../../include/data/client/MetaDataLocationObtainer.h"
#include "../../../include/data/client/../constructs/server/RangeDefinition.h"
#include "../../../include/data/client/../constructs/StructureDefinitions.h"
#include "../../../include/data/client/../../interconnect/TabletServer.h"
#include "../../../include/data/client/../../interconnect/Scan.h"
#include "../../../include/data/client/../constructs/Range.h"
#include "../../../include/data/client/../constructs/KeyExtent.h"
#include "../../../include/data/client/../constructs/Key.h"
#include "../../../include/data/client/../constructs/IterInfo.h"
#include "../../../include/data/client/../constructs/value.h"
#include "../../../include/data/client/TabletLocation.h"

namespace cclient
{
namespace impl
{


MetaDataLocationObtainer::~MetaDataLocationObtainer ()
{
    for (std::vector<cclient::data::Column*>::iterator locIt = columns->begin ();
            locIt != columns->end (); locIt++)
    {
        delete (*locIt);
    }

    delete columns;

}

std::vector<cclient::data::TabletLocation>
MetaDataLocationObtainer::findTablet (cclient::data::security::AuthInfo *credentials,
                                      cclient::data::TabletLocation *source, std::string row,
                                      std::string stopRow, TabletLocator *parent)
{
    std::vector<cclient::data::TabletLocation> tabletLocations;
    cclient::data::Key startKey;
    startKey.setRow (row.c_str (), row.size ());
    cclient::data::Key endKey;
    endKey.setRow (stopRow.c_str (), stopRow.size ());
    cclient::data::Range *range = new cclient::data::Range (&startKey, true, &endKey, true);

    std::map<cclient::data::Key, cclient::data::Value> resultSet;

    cclient::data::security::Authorizations emptyAuths;

    std::vector<cclient::data::Range*> ranges;
    ranges.push_back (range);

    std::vector<cclient::data::KeyExtent*> extents;
    extents.push_back (source->getExtent ());
    cclient::data::tserver::RangeDefinition *rangeDef = new cclient::data::tserver::RangeDefinition (credentials, &emptyAuths,
            source->getServer (),
            source->getPort (),
            &ranges, &extents);

    Configuration conf;
    interconnect::ServerInterconnect *directConnect =
        new interconnect::ServerInterconnect (rangeDef, &conf);

    cclient::data::IterInfo wriIter (
        "WRI", "org.apache.accumulo.core.iterators.user.WholeRowIterator",
        10000);

    std::vector<cclient::data::IterInfo*> iters;
    iters.push_back (&wriIter);
    interconnect::Scan *initScan = directConnect->scan (columns, &iters);
    std::vector<cclient::data::KeyValue*> kvResults;
    initScan->getNextResults (&kvResults);

    std::map<cclient::data::Key*, cclient::data::Value*, pointer_comparator<cclient::data::Key*>> results = decodeResults (
                &kvResults);

    cclient::data::Key *key = 0;
    cclient::data::Value * value = 0;
    std::string lastRowFromKey = "";
    std::string currentRow = "";

    std::string location = "", session = "";

    cclient::data::Value *prevRow = 0;

    for (std::map<cclient::data::Key*, cclient::data::Value*>::iterator it = results.begin ();
            it != results.end (); it++)
    {
        key = it->first;
        currentRow = std::string (key->getRow ().first, key->getRow ().second);
        if (currentRow != lastRowFromKey)
        {
            prevRow = 0;
            location = session = "";
            lastRowFromKey = currentRow;

        }

        std::pair<char*, size_t> cfBytes = key->getColFamily ();
        std::string cf = std::string (cfBytes.first, cfBytes.second);
        std::pair<char*, size_t> cqBytes = key->getColQualifier ();
        std::string cq = std::string (cqBytes.first, cqBytes.second);

        value = it->second;
        std::pair<uint8_t*, size_t> valBytes = value->getValue ();
        if (cf == METADATA_CURRENT_LOCATION_COLUMN_FAMILY
                || cf == METADATA_FUTURE_LOCATION_COLUMN_FAMILY)
        {
            location = std::string ((char*) valBytes.first, valBytes.second);
            session = cq;
        } 
        else if (cf == METADATA_TABLET_COLUMN_FAMILY
                 && cq == METADATA_PREV_ROW_COLUMN_CQ)
        {
            prevRow = value;
        }

        if (prevRow != NULL)
        {

            cclient::data::KeyExtent ke(currentRow, prevRow);
            if (location.length () > 0)
            {

                cclient::data::TabletLocation te(&ke, location,
                        session);
		

                tabletLocations.push_back (te);
            }
        }

    }

    for (std::map<cclient::data::Key*, cclient::data::Value*>::iterator it = results.begin ();
            it != results.end (); it++)
    {
        delete it->first;
        delete it->second;
    }

    delete initScan;

    // cleanup
   // delete rangeDef;
    
    
    delete range;
    
    delete directConnect;

    return tabletLocations;
}

} /* namespace impl */
} /* namespace cclient */

