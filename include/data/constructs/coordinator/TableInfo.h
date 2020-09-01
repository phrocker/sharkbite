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
namespace cclient
{
    namespace data
    {

        class TableRates
        {
        public:
            double ingestRate;
            double ingestByteRate;
            double queryRate;
            double queryByteRate;
            double scanRate;

            explicit TableRates(double ingestRate, double ingestByteRate, double queryRate, double queryByteRate, double scanRate) : ingestRate(ingestRate), ingestByteRate(ingestByteRate), queryRate(queryRate), queryByteRate(queryByteRate), scanRate(scanRate)
            {
            }

            auto getIngestRate() const
            {
                return ingestRate;
            }

            auto getIngestRateByte() const
            {
                return ingestByteRate;
            }

            auto getQueryRate() const
            {
                return queryRate;
            }

            auto getQueryRateByte() const
            {
                return queryByteRate;
            }

            auto getScanRate() const
            {
                return scanRate;
            }
        };

        class Compacting
        {
        public:
            int32_t running;
            int32_t queued;

            explicit Compacting(int32_t running, int32_t queued) : running(running), queued(queued)
            {
            }

            auto getRunning() const
            {
                return running;
            }

            auto getQueued() const
            {
                return queued;
            }
        };

        class TableCompactions
        {
        public:
            Compacting minors;
            Compacting majors;
            Compacting scans;

            explicit TableCompactions(Compacting minors, Compacting majors, Compacting scans) : minors(minors), majors(majors), scans(scans)
            {
            }

            auto getMinorCompactions() const
            {
                return minors;
            }

            auto getMajorCompactions() const
            {
                return majors;
            }

            auto getScans() const
            {
                return scans;
            }
        };

        class TableInfo
        {
        public:
            int64_t recs;
            int64_t recsInMemory;
            int32_t tablets;
            int32_t onlineTablets;
            TableRates rates;
            TableCompactions compactionInfo;

            explicit TableInfo(
                int64_t recs, int64_t recsInMemory, int32_t tablets, int32_t onlineTablets, TableRates rates, TableCompactions compactions) : recs(recs), recsInMemory(recsInMemory), tablets(tablets), onlineTablets(onlineTablets), rates(rates), compactionInfo(compactions)
            {
            }

            auto getRecords() const
            {
                return recs;
            }

            auto getRecordsInMemory() const
            {
                return recsInMemory;
            }

            auto getTablets() const
            {
                return tablets;
            }

            auto getOnlineTablets() const
            {
                return onlineTablets;
            }

            auto getTableRates() const
            {
                return rates;
            }

            auto getCompactioninfo() const
            {
                return compactionInfo;
            }
        };

    } // namespace data
} // namespace cclient