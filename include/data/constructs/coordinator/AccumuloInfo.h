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

#pragma once

#include <map>
#include <vector>
#include <set>
#include <string>
#include "TableInfo.h"
#include "TabletServerStatus.h"

namespace cclient
{
    namespace data
    {

        class DeadServer
        {
        public:
            std::string server;
            int64_t lastStatus;
            std::string status;
            explicit DeadServer(const std::string &server, int64_t last, std::string status) : server(server), lastStatus(last), status(status)
            {
            }

            auto getServer() const
            {
                return server;
            }

            auto getLastContact() const
            {
                return lastStatus;
            }

            auto getStatus() const
            {
                return status;
            }
        };

        struct CoordinatorState
        {
            enum type
            {
                INITIAL = 0,
                HAVE_LOCK = 1,
                SAFE_MODE = 2,
                NORMAL = 3,
                UNLOAD_METADATA_TABLETS = 4,
                UNLOAD_ROOT_TABLET = 5,
                STOP = 6
            };
        };

        struct CoordinatorGoalState
        {
            enum type
            {
                CLEAN_STOP = 0,
                SAFE_MODE = 1,
                NORMAL = 2
            };
        };

        class AccumuloInfoBuilder;

        /**
 * Main accumulo statistics object that provides statistics from
 * the coordinator
 */
        class AccumuloInfo
        {
        public:
            static AccumuloInfoBuilder make();

        private:
            friend class AccumuloInfoBuilder;
            /**
     * Constructor that is private requiring the use
     * of the builder
     */
            AccumuloInfo() : unassignedTablets(0)
            {
            }

        public:
            std::map<std::string, TableInfo> tableMap;
            std::vector<TabletServerStatus> tServerInfo;
            std::map<std::string, int8_t> badTServers;
            CoordinatorState::type state;
            CoordinatorGoalState::type goalState;
            int32_t unassignedTablets;
            std::set<std::string> serversShuttingDown;
            std::vector<DeadServer> deadTabletServers;

            auto getTableMap() const
            {
                return tableMap;
            }

            auto getTabletServerInfo() const
            {
                return tServerInfo;
            }

            auto getBadTabletServers() const
            {
                return badTServers;
            }

            auto getState() const
            {
                return state;
            }

            auto getGoalState() const
            {
                return goalState;
            }

            auto getUnassignedTablets() const
            {
                return unassignedTablets;
            }

            auto getServerShuttingDown() const
            {
                return serversShuttingDown;
            }

            auto getDeadServers() const
            {
                return deadTabletServers;
            }
        };

        class AccumuloInfoBuilder
        {
        public:
            AccumuloInfoBuilder &tableMap(const std::map<std::string, TableInfo> &tableMap)
            {
                info.tableMap = tableMap;
                return *this;
            }

            AccumuloInfoBuilder &tabletServerInfo(const std::vector<TabletServerStatus> &tServerInfo)
            {
                info.tServerInfo = tServerInfo;
                return *this;
            }

            AccumuloInfoBuilder &badTabletServers(const std::map<std::string, int8_t> &badTServers)
            {
                info.badTServers = badTServers;
                return *this;
            }

            AccumuloInfoBuilder &state(CoordinatorState::type state)
            {
                info.state = state;
                return *this;
            }

            AccumuloInfoBuilder &goalState(CoordinatorGoalState::type goalState)
            {
                info.goalState = goalState;
                return *this;
            }

            AccumuloInfoBuilder &unassignedTablets(int32_t unassignedTablets)
            {
                info.unassignedTablets = unassignedTablets;
                return *this;
            }

            AccumuloInfoBuilder &serversShuttingDown(const std::set<std::string> &serversShuttingDown)
            {
                info.serversShuttingDown = serversShuttingDown;
                return *this;
            }

            AccumuloInfoBuilder &deadTabletServers(const std::vector<DeadServer> &deadTabletServers)
            {
                info.deadTabletServers = deadTabletServers;
                return *this;
            }

            operator AccumuloInfo &&()
            {
                return std::move(info);
            }

        private:
            AccumuloInfo info;
        };

    } // namespace data

} // namespace cclient
