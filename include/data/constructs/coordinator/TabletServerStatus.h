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
#include "TableInfo.h"
namespace cclient {
namespace data {

    

class RecoveryStatus{
private:
    std::string name;
    int32_t runtime;  
    double progress;
public:
    explicit RecoveryStatus(std::string name,
    int32_t runtime,  
    double progress) : name(name),runtime(runtime),progress(progress){
    }

    auto getName() const{
        return name;
    }

    auto getRuntime() const{
        return runtime;
    }

    auto getProgress() const{
        return progress;
    }
     
};

class TabletServerStatusBuilder;

class TabletServerStatus{
public:
    static TabletServerStatusBuilder make();

    auto getTableMap() const{
        return tableMap;
    }

    auto getLastContact() const{
        return lastContact;
    }

    auto getName() const{
        return name;
    }

    auto getOsLoad() const{
        return osLoad;
    }

    auto getLookups() const{
        return lookups;
    }

    auto getIndexCacheHits() const{
        return indexCacheHits;
    }

    auto getDataCacheHits() const{
        return dataCacheHits;
    }

    auto getDataCacheRequests() const{
        return dataCacheRequest;
    }

    auto getLogSorts() const{
        return logSorts;
    }

    auto getFlushes() const{
        return flushs;
    }

    auto getSyncs() const{
        return syncs;
    }
private:
    friend class TabletServerStatusBuilder;
    TabletServerStatus():lastContact(0),holdTime(0),lookups(0),indexCacheHits(0),indexCacheRequest(0),dataCacheHits(0),dataCacheRequest(0),flushs(0),syncs(0){
    
    }
    std::map<std::string, cclient::data::TableInfo>  tableMap;
    int64_t lastContact;
    std::string name;
    double osLoad;
    int64_t holdTime;
    int64_t lookups;
    int64_t indexCacheHits;
    int64_t indexCacheRequest;
    int64_t dataCacheHits;
    int64_t dataCacheRequest;
    std::vector<RecoveryStatus>  logSorts;
    int64_t flushs;
    int64_t syncs;
};

class TabletServerStatusBuilder{
    public:
        TabletServerStatusBuilder& tableMap(std::map<std::string, TableInfo> map){
            status.tableMap = map;
            return *this;
        }

        TabletServerStatusBuilder& lastContact(int64_t lastContact){
            status.lastContact = lastContact;
            return *this;
        }


        TabletServerStatusBuilder& name(std::string name){
            status.name = name;
            return *this;
        }

        TabletServerStatusBuilder& osLoad(double osLoad){
            status.osLoad = osLoad;
            return *this;
        }

        TabletServerStatusBuilder& holdTime(int64_t holdTime){
            status.holdTime = holdTime;
            return *this;
        }

        TabletServerStatusBuilder& lookups(int64_t lookups){
            status.lookups = lookups;
            return *this;
        }

        TabletServerStatusBuilder& indexCacheHits(int64_t indexCacheHits){
            status.indexCacheHits = indexCacheHits;
            return *this;
        }

        TabletServerStatusBuilder& indexCacheRequest(int64_t indexCacheRequest){
            status.indexCacheRequest = indexCacheRequest;
            return *this;
        }

        TabletServerStatusBuilder& dataCacheHits(int64_t dataCacheHits){
            status.dataCacheHits = dataCacheHits;
            return *this;
        }

        TabletServerStatusBuilder& dataCacheRequest(int64_t dataCacheRequest){
            status.dataCacheRequest = dataCacheRequest;
            return *this;
        }

        TabletServerStatusBuilder& logSorts(std::vector<RecoveryStatus> logSorts){
            status.logSorts = logSorts;
            return *this;
        }

        TabletServerStatusBuilder& flushs(int64_t flushs){
            status.flushs = flushs;
            return *this;
        }

         TabletServerStatusBuilder& syncs(int64_t syncs){
            status.syncs = syncs;
            return *this;
        }

        


        operator TabletServerStatus&&() {
            return std::move(status);
        }
        
            
    private:
    TabletServerStatus status;
};



}
}