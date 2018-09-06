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
#include "data/constructs/client/TabletServerMutations.h"

namespace cclient
{
namespace data
{

TabletServerMutations::TabletServerMutations (std::string sessionId) :
    session (sessionId), failuresAllowed(-1)
{
    

}

TabletServerMutations::TabletServerMutations (std::string sessionId, uint32_t fails) :
    session (sessionId), failuresAllowed(fails)
{
    

}

TabletServerMutations::~TabletServerMutations ()
{
    for (auto it : mutations)
    {

        for (auto mutation : it.second)
        {
            delete mutation;
        }
    }
}

} /* namespace data */
} /* namespace cclient */
