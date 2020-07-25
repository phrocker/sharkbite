#ifndef THRIFTWRAPPER_H_
#define THRIFTWRAPPER_H_

#include <algorithm>
#include <map>
#include <set>

#include "../../constructs/security/AuthInfo.h"
#include "../../constructs/security/Authorizations.h"
#include "../../constructs/inputvalidation.h"
#include "../../constructs/IterInfo.h"
#include "../../constructs/Key.h"
#include "../../constructs/Range.h"
#include "../../constructs/KeyValue.h"
#include "../../constructs/Mutation.h"
#include "../../constructs/KeyExtent.h"
#include "../../constructs/column.h"
#include "data/constructs/coordinator/AccumuloInfo.h"

#include "gzip/compress.hpp"
#include "gzip/config.hpp"
#include "gzip/decompress.hpp"
#include "gzip/utils.hpp"
#include "gzip/version.hpp"
#include "../../streaming/ByteOutputStream.h"
#include "../../streaming/NetworkOrderStream.h"
#include "../../streaming/input/NetworkOrderInputStream.h"
#include "data_types.h"
#include "security_types.h"
namespace interconnect {

class ThriftWrapper {
 protected:
  static std::string decompress(const char *compressed, uint32_t size) {
    return gzip::decompress(compressed, size);
  }
 public:

  static std::shared_ptr<cclient::data::KeyExtent> convert(::org::apache::accumulo::core::data::thrift::TKeyExtent extent) {
    return std::make_shared<cclient::data::KeyExtent>(extent.table, extent.endRow, extent.prevEndRow);

  }
  static std::vector<cclient::data::Column> convert(std::vector<::org::apache::accumulo::core::data::thrift::TColumn> columns) {
    std::vector<cclient::data::Column> cols;
    for (auto it = columns.begin(); it != columns.end(); it++) {
      cols.emplace_back(cclient::data::Column((*it).columnFamily, (*it).columnQualifier, (*it).columnVisibility));
    }

    return cols;
  }

  static std::vector<cclient::data::IterInfo> convert(std::vector<::org::apache::accumulo::core::data::thrift::IterInfo> iters) {
    std::vector<cclient::data::IterInfo> convertedIters;
    for (auto it = iters.begin(); it != iters.end(); it++) {
      convertedIters.emplace_back(cclient::data::IterInfo((*it).iterName, (*it).className, (*it).priority));
    }

    return convertedIters;
  }
  static cclient::data::security::AuthInfo *convert(org::apache::accumulo::core::security::thrift::TCredentials creds) {
    cclient::data::streams::EndianInputStream inputStream((char*) creds.token.c_str(), creds.token.length());
    int compressedLength = inputStream.readInt();
    char *array = new char[compressedLength];
    std::string decompressed = decompress(array, compressedLength);
    delete[] array;
    return new cclient::data::security::AuthInfo(creds.principal, decompressed, creds.instanceId);
  }

  static org::apache::accumulo::core::security::thrift::TCredentials convert(cclient::data::security::AuthInfo *authInfo) {
    org::apache::accumulo::core::security::thrift::TCredentials stackAuthInfo;
    stackAuthInfo.instanceId = authInfo->getInstanceId();
    // password token needs to be compressed

    std::string compressedString;
    {
      auto uncompressedPassword = authInfo->getPassword();
      compressedString =  gzip::compress(uncompressedPassword.data(), uncompressedPassword.size());
    }

    cclient::data::streams::BigEndianByteStream writeStream(0);
    writeStream.writeInt(compressedString.size());
    writeStream.write(compressedString.c_str(), compressedString.size());

    stackAuthInfo.tokenClassName = "org.apache.accumulo.core.client.security.tokens.PasswordToken";
    stackAuthInfo.principal = authInfo->getUserName();

    stackAuthInfo.token = std::string(writeStream.getByteArray(), writeStream.getPos());
    return stackAuthInfo;
  }

  static std::vector<org::apache::accumulo::core::data::thrift::IterInfo> convert(const std::vector<cclient::data::IterInfo> &iters) {

    std::vector<org::apache::accumulo::core::data::thrift::IterInfo> convertedIters;
    if (!iters.empty()) {
      for( const auto &iter : iters){
        org::apache::accumulo::core::data::thrift::IterInfo iterInfo;
        iterInfo.className = iter.getClass();
        iterInfo.iterName = iter.getName();
        iterInfo.priority = iter.getPriority();
        convertedIters.push_back(iterInfo);
      }
    }

    return convertedIters;

  }

  static std::vector<org::apache::accumulo::core::data::thrift::TMutation> convert(std::vector<std::shared_ptr<cclient::data::Mutation>> *iters) {

    std::vector<org::apache::accumulo::core::data::thrift::TMutation> convertedMutations;
    if (!IsEmpty(iters)) {
      for (auto it = iters->begin(); it != iters->end(); it++) {
        org::apache::accumulo::core::data::thrift::TMutation mut;
        mut.row = (*it)->getRow();

        mut.data = (*it)->getDataStr();
        mut.entries = (*it)->size();
        convertedMutations.push_back(mut);
      }
    }

    return convertedMutations;

  }

  static std::shared_ptr<cclient::data::Range> convert(org::apache::accumulo::core::data::thrift::TRange range) {
    std::shared_ptr<cclient::data::Key> startKey = convert(range.start);
    std::shared_ptr<cclient::data::Key> endKey = convert(range.stop);
    return std::make_shared<cclient::data::Range>(startKey, range.startKeyInclusive, endKey, range.stopKeyInclusive);

  }

  static org::apache::accumulo::core::data::thrift::ScanBatch convert(std::vector<interconnect::ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, std::shared_ptr<cclient::data::Range>> *> *rangeIdentifiers) {

    org::apache::accumulo::core::data::thrift::ScanBatch littleBatch;

    //typedef std::map<class TKeyExtent, std::std::vector<class Tcclient::data::Range> >  ScanBatch;
    for (auto it : *rangeIdentifiers) {

      interconnect::ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, std::shared_ptr<cclient::data::Range>> *identifier = it;
      std::vector<std::shared_ptr<cclient::data::KeyExtent> > extents = identifier->getGlobalMapping();
      for (std::shared_ptr<cclient::data::KeyExtent> ot : extents) {
        org::apache::accumulo::core::data::thrift::TKeyExtent keyExtent;

        keyExtent.table = ot->getTableId();
        std::string endRow = ot->getEndRow();
        if (!IsEmpty(&endRow))
          keyExtent.endRow = ot->getEndRow();

        std::string prevEndRow = ot->getEndRow();
        if (!IsEmpty(&prevEndRow))
          keyExtent.prevEndRow = ot->getPrevEndRow();

        std::vector<org::apache::accumulo::core::data::thrift::TRange> thriftRanges;
        auto ranges = identifier->getIdentifiers(ot);
        for (const auto &rt : ranges) {

          org::apache::accumulo::core::data::thrift::TRange range = convert(rt);
          thriftRanges.push_back(range);

        }
        littleBatch.insert(std::pair<org::apache::accumulo::core::data::thrift::TKeyExtent, std::vector<org::apache::accumulo::core::data::thrift::TRange>>(keyExtent, thriftRanges));
      }

    }

    return littleBatch;
  }

  static org::apache::accumulo::core::data::thrift::TKey convert(std::shared_ptr<cclient::data::Key> key) {
    org::apache::accumulo::core::data::thrift::TKey newKey;
    if (NULL != key) {
      std::pair<char*, size_t> rowPair = key->getRow();
      newKey.row = std::string(rowPair.first, rowPair.second);

      std::pair<char*, size_t> cfPair = key->getColFamily();
      if (cfPair.second > 0)
        newKey.colFamily = std::string(cfPair.first, cfPair.second);

      std::pair<char*, size_t> cqPair = key->getColQualifier();
      if (cqPair.second > 0)
        newKey.colQualifier = std::string(cqPair.first, cqPair.second);

      std::pair<char*, size_t> cvPair = key->getColVisibility();
      if (cvPair.second > 0)
        newKey.colVisibility = std::string(cvPair.first, cvPair.second);

      newKey.timestamp = key->getTimeStamp();
    } else {
      newKey.row = "";
      newKey.colFamily = "";
      newKey.colQualifier = "";
      newKey.colVisibility = "";

    }

    return newKey;
  }

  static std::shared_ptr<cclient::data::Key> convert(org::apache::accumulo::core::data::thrift::TKey key) {
    std::shared_ptr<cclient::data::Key> newKey = std::make_shared<cclient::data::Key>();
    newKey->setRow(key.row);
    newKey->setColFamily(key.colFamily);
    newKey->setColQualifier(key.colQualifier);
    newKey->setColVisibility(key.colVisibility);
    newKey->setTimeStamp(key.timestamp);
    return newKey;
  }

  static org::apache::accumulo::core::data::thrift::TKeyExtent convert(std::shared_ptr<cclient::data::KeyExtent> ot) {
    org::apache::accumulo::core::data::thrift::TKeyExtent keyExtent;
    keyExtent.table = ot->getTableId();
    std::string endRow = ot->getEndRow();

    if (!IsEmpty(&endRow))
      keyExtent.endRow = endRow;
    else
      keyExtent.endRow = "";

    std::string prevEndRow = ot->getPrevEndRow();

    if (!IsEmpty(&prevEndRow))
      keyExtent.prevEndRow = prevEndRow;
    else
      keyExtent.prevEndRow = "";

    return keyExtent;
  }

  static org::apache::accumulo::core::data::thrift::TKeyExtent convert(const cclient::data::KeyExtent ot) {
    org::apache::accumulo::core::data::thrift::TKeyExtent keyExtent;
    keyExtent.table = ot.getTableId();
    std::string endRow = ot.getEndRow();

    if (!IsEmpty(&endRow))
      keyExtent.endRow = endRow;
    else
      keyExtent.endRow = "";

    std::string prevEndRow = ot.getPrevEndRow();

    if (!IsEmpty(&prevEndRow))
      keyExtent.prevEndRow = prevEndRow;
    else
      keyExtent.prevEndRow = "";

    return keyExtent;
  }

  static std::vector<std::shared_ptr<cclient::data::KeyValue> > *convert(std::vector<org::apache::accumulo::core::data::thrift::TKeyValue> tkvVec) {
    std::vector<std::shared_ptr<cclient::data::KeyValue>> *newvector = new std::vector<std::shared_ptr<cclient::data::KeyValue>>();
    std::shared_ptr<cclient::data::Key> prevKey = NULL;

    for (uint32_t i = 0; i < tkvVec.size(); i++) {

      org::apache::accumulo::core::data::thrift::TKeyValue tkv = tkvVec.at(i);
      std::shared_ptr<cclient::data::Key> key = std::make_shared<cclient::data::Key>();
      if (!IsEmpty(&(tkv.key.row))) {
        key->setRow(tkv.key.row.c_str(), tkv.key.row.size());
      } else {
        if (NULL != prevKey) {
          std::pair<char*, size_t> prevRow = prevKey->getRow();
          key->setRow(prevRow.first, prevRow.second);
        }

      }
      if (!IsEmpty(&(tkv.key.colFamily))) {
        key->setColFamily(tkv.key.colFamily.c_str(), tkv.key.colFamily.size());
      } else {
        if (NULL != prevKey) {
          std::pair<char*, size_t> prevCf = prevKey->getColFamily();
          key->setColFamily(prevCf.first, prevCf.second);
        }
      }
      if (!IsEmpty(&(tkv.key.colQualifier))) {
        key->setColQualifier(tkv.key.colQualifier.c_str(), tkv.key.colQualifier.size());
      } else {
        if (NULL != prevKey) {
          std::pair<char*, size_t> prevCq = prevKey->getColQualifier();
          key->setColQualifier(prevCq.first, prevCq.second);
        }
      }

      if (!IsEmpty(&(tkv.key.colVisibility))) {
        key->setColVisibility(tkv.key.colVisibility.c_str(), tkv.key.colVisibility.size());
      } else {
        if (NULL != prevKey) {
          std::pair<char*, size_t> prevCv = prevKey->getColVisibility();
          key->setColVisibility(prevCv.first, prevCv.second);
        }
      }

      key->setTimeStamp(tkv.key.timestamp);
      std::shared_ptr<cclient::data::KeyValue> kv = std::make_shared<cclient::data::KeyValue>();
      kv->setKey(key, true);
      kv->setValue((uint8_t*) tkv.value.c_str(), tkv.value.size());

      newvector->push_back(kv);

      prevKey = key;
    }
    return newvector;
  }

  static org::apache::accumulo::core::data::thrift::TRange convert(const std::shared_ptr<cclient::data::Range> &range) {
    org::apache::accumulo::core::data::thrift::TRange newRange;
    if (!range->getInfiniteStartKey() && NULL != range->getStartKey())
      newRange.start = convert(range->getStartKey());

    if (!range->getInfiniteStopKey() && NULL != range->getStopKey())
      newRange.stop = convert(range->getStopKey());

    newRange.startKeyInclusive = range->getStartKeyInclusive();
    newRange.stopKeyInclusive = range->getStopKeyInclusive();
    newRange.infiniteStartKey = range->getInfiniteStartKey();
    newRange.infiniteStopKey = range->getInfiniteStopKey();
    return newRange;
  }

  static std::vector<org::apache::accumulo::core::data::thrift::TColumn> convert(const std::vector<cclient::data::Column> &columns) {

    std::vector<org::apache::accumulo::core::data::thrift::TColumn> convertedColumns;

    if (!columns.empty()) {
      for( const auto &col : columns){
        org::apache::accumulo::core::data::thrift::TColumn column;
        column.columnFamily = col.getColFamily();
        column.columnQualifier = col.getColQualifier();
        column.columnVisibility = col.getColVisibility();

        convertedColumns.push_back(column);
      }
    }

    return convertedColumns;

  }

  static std::vector<std::string> convert(cclient::data::security::Authorizations *auths) {

    return auths->getAuthorizations();

  }

  static std::map<std::string,cclient::data::TableInfo> convert(std::map<std::string, org::apache::accumulo::core::master::thrift::TableInfo> tableMap){  
    std::map<std::string,cclient::data::TableInfo> ret;

  	std::for_each(tableMap.begin(), tableMap.end(),
				[&ret] (const std::pair<std::string,org::apache::accumulo::core::master::thrift::TableInfo> &entry)
				{
          auto tableRates = cclient::data::TableRates(entry.second.ingestRate, entry.second.ingestByteRate,
                entry.second.queryRate, entry.second.queryByteRate);
          auto tableCompacting = cclient::data::TableCompactions(
                cclient::data::Compacting(entry.second.minors.running,entry.second.minors.queued),
                cclient::data::Compacting(entry.second.majors.running,entry.second.majors.queued),;
                cclient::data::Compacting(entry.second.scans.running,entry.second.scans.queued));
              
					ret.insert( std::make_pair(entry.first,
          cclient::data::TableInfo( entry.second.recs,entry.second.recsInMemory,
              entry.second.tablets, entry.second.onlineTablets, 
              tableRates,tableCompacting)));
				});
        return ret;
  }

  static std::vector<cclient::data::TabletServerStatus> convert(std::vector<org::apache::accumulo::core::master::thrift::TabletServerStatus> stat){

    std::vector<cclient::data::TabletServerStatus> ret;

  	std::for_each(stat.begin(), stat.end(),
				[&ret] (const org::apache::accumulo::core::master::thrift::TabletServerStatus &entry)
				{
					ret.push_back(
            cclient::data::TabletServerStatus::make().tableMap(convert(entry.tableMap)).lastContact(entry.lastContact).
              name(entry.name).osLoad(entry.osLoad).holdTime(entry.holdTime).lookups(entry.lookups).indexCacheHits(entry.indexCacheHits).
              indexCacheRequest(entry.indexCacheRequest).dataCacheHits(entry.dataCacheHits).dataCacheRequest(entry.dataCacheRequest).
              logSorts(entry.logSorts).flushs(entry.flushs).syncs(entry.syncs)
            );
				});
        return ret;

    return ret;
  }

  static cclient::data::CoordinatorState::type convert(org::apache::accumulo::core::master::thrift::MasterState::type state){
    switch(state){
      case org::apache::accumulo::core::master::thrift::MasterState::type::INITIAL:
        return cclient::data::CoordinatorState::type::INITIAL;
      case org::apache::accumulo::core::master::thrift::MasterState::type::HAVE_LOCK:
        return cclient::data::CoordinatorState::type::HAVE_LOCK;
      case org::apache::accumulo::core::master::thrift::MasterState::type::SAFE_MODE:
        return cclient::data::CoordinatorState::type::SAFE_MODE;
      case org::apache::accumulo::core::master::thrift::MasterState::type::NORMAL:
        return cclient::data::CoordinatorState::type::NORMAL;
      case org::apache::accumulo::core::master::thrift::MasterState::type::UNLOAD_METADATA_TABLETS:
        return cclient::data::CoordinatorState::type::UNLOAD_METADATA_TABLETS;
      case org::apache::accumulo::core::master::thrift::MasterState::type::UNLOAD_ROOT_TABLET:
        return cclient::data::CoordinatorState::type::UNLOAD_ROOT_TABLET;
      case org::apache::accumulo::core::master::thrift::MasterState::type::STOP:
        return cclient::data::CoordinatorState::type::STOP;  
      default:
      return cclient::data::CoordinatorState::type::INITIAL;
    };
  }

  static cclient::data::CoordinatorGoalState::type convert(org::apache::accumulo::core::master::thrift::MasterGoalState::type state){
    switch(state){
      case org::apache::accumulo::core::master::thrift::MasterGoalState::type::CLEAN_STOP:
        return cclient::data::CoordinatorGoalState::type::CLEAN_STOP;
      case org::apache::accumulo::core::master::thrift::MasterGoalState::type::SAFE_MODE:
        return cclient::data::CoordinatorGoalState::type::SAFE_MODE;
      case org::apache::accumulo::core::master::thrift::MasterGoalState::type::NORMAL:
        return cclient::data::CoordinatorGoalState::type::NORMAL;
      default:
      return cclient::data::CoordinatorGoalState::type::NORMAL;
    };
  }

  static std::vector<cclient::data::DeadServer>  convert(std::vector<org::apache::accumulo::core::master::thrift::DeadServer>  deadTabletServers){
    std::vector<cclient::data::DeadServer> ret;

	std::for_each(deadTabletServers.begin(), deadTabletServers.end(),
				[&ret] (const org::apache::accumulo::core::master::thrift::DeadServer &entry)
				{
					ret.push_back(cclient::data::DeadServer(entry.server,entry.lastStatus,entry.status));
				});
        return ret;
  }


  static cclient::data::AccumuloInfo convert(org::apache::accumulo::core::master::thrift::MasterMonitorInfo &stats){
    return cclient::data::AccumuloInfo::make().
        tableMap( convert(stats.tableMap) ).tabletServerInfo( convert(stats.tServerInfo)).
        badTabletServers(stats.badTServers).state(convert(stats.state)).
        goalState(convert(stats.goalState)).unassignedTablets(stats.unassignedTablets).
        serversShuttingDown(stats.serversShuttingDown).deadTabletServers(convert(stats.deadTabletServers));
  }

 protected:
  ThriftWrapper() {

  }
  virtual ~ThriftWrapper() {

  }
}
;

} /* namespace interconnect */
#endif /* THRIFTWRAPPER_H_ */

