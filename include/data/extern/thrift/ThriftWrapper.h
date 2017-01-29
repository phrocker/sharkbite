#ifndef THRIFTWRAPPER_H_
#define THRIFTWRAPPER_H_

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
#include <boost/iostreams/filter/gzip.hpp>
#include <boost/iostreams/filtering_stream.hpp>

#include "../../streaming/ByteOutputStream.h"
#include "../../streaming/NetworkOrderStream.h"
#include "../../streaming/input/NetworkOrderInputStream.h"
#include "data_types.h"
#include "security_types.h"
namespace interconnect
{


class ThriftWrapper
{
protected:
	static std::string decompress(const char *compressed, uint32_t size)
	{
		std::string decompressed;

		boost::iostreams::filtering_ostream os;

		os.push(boost::iostreams::gzip_decompressor());
		os.push(boost::iostreams::back_inserter(decompressed));

		boost::iostreams::write(os, compressed, size);

		return decompressed;
	}
public:

	static cclient::data::KeyExtent *convert( ::org::apache::accumulo::core::data::thrift::TKeyExtent extent)
	{
		return new cclient::data::KeyExtent(extent.table,extent.endRow,extent.prevEndRow);

	}
	static std::vector<cclient::data::Column*> convert( std::vector< ::org::apache::accumulo::core::data::thrift::TColumn> columns)
	{
		std::vector<cclient::data::Column*> cols;
		for(auto it = columns.begin(); it != columns.end(); it++) {
			cols.push_back(new cclient::data::Column((*it).columnFamily,(*it).columnQualifier,(*it).columnVisibility));
		}

		return cols;
	}

	static std::vector<cclient::data::IterInfo*> convert( std::vector<::org::apache::accumulo::core::data::thrift::IterInfo> iters)
	{
		std::vector<cclient::data::IterInfo*> convertedIters;
		for (auto it = iters.begin(); it != iters.end(); it++) {
			cclient::data::IterInfo *iterInfo = new cclient::data::IterInfo( (*it).iterName, (*it).className, (*it).priority );
			convertedIters.push_back(iterInfo);
		}

		return convertedIters;
	}
	static cclient::data::security::AuthInfo *convert( org::apache::accumulo::core::security::thrift::TCredentials creds)
	{
		cclient::data::streams::EndianInputStream inputStream((char*)creds.token.c_str(),creds.token.length());
		int compressedLength = inputStream.readInt();
		char *array = new char[ compressedLength ];
		std::string decompressed = decompress( array, compressedLength );
		delete [] array;
		return new cclient::data::security::AuthInfo(creds.principal,decompressed,creds.instanceId);
	}
	static org::apache::accumulo::core::security::thrift::TCredentials convert(
	        cclient::data::security::AuthInfo *authInfo)
	{
		org::apache::accumulo::core::security::thrift::TCredentials stackAuthInfo;
		stackAuthInfo.instanceId = authInfo->getInstanceId();
		// password token needs to be compressed

		std::string compressedString;
		{
			boost::iostreams::filtering_ostream compressingStream;
			compressingStream.push(
			        boost::iostreams::gzip_compressor(
			                boost::iostreams::gzip_params(boost::iostreams::gzip::best_compression)));
			compressingStream.push(
			        boost::iostreams::back_inserter(compressedString));
			compressingStream << authInfo->getPassword();
			boost::iostreams::close(compressingStream);
		}

		cclient::data::streams::BigEndianByteStream writeStream(0);
		writeStream.writeInt(compressedString.size());
		writeStream.write(compressedString.c_str(), compressedString.size());

		stackAuthInfo.tokenClassName =
		        "org.apache.accumulo.core.client.security.tokens.PasswordToken";
		stackAuthInfo.principal = authInfo->getUserName();

		stackAuthInfo.token = std::string(writeStream.getByteArray(),
		                             writeStream.getPos());
		return stackAuthInfo;
	}



	static std::vector<org::apache::accumulo::core::data::thrift::IterInfo> convert(
	        std::vector<cclient::data::IterInfo*> *iters)
	{

		std::vector<org::apache::accumulo::core::data::thrift::IterInfo> convertedIters;
		if (!IsEmpty(iters)) {
			for (auto it = iters->begin(); it != iters->end(); it++) {
				org::apache::accumulo::core::data::thrift::IterInfo iterInfo;
				iterInfo.className = (*it)->getClass();
				iterInfo.iterName = (*it)->getName();
				iterInfo.priority = (*it)->getPriority();
				convertedIters.push_back(iterInfo);
			}
		}

		return convertedIters;

	}

	static std::vector<org::apache::accumulo::core::data::thrift::TMutation> convert(
	        std::vector<cclient::data::Mutation*> *iters)
	{

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

	static cclient::data::Range* convert(org::apache::accumulo::core::data::thrift::TRange range)
	{
		cclient::data::Key *startKey = convert(range.start);
		cclient::data::Key *endKey = convert(range.stop);
		return new cclient::data::Range(startKey,range.startKeyInclusive,endKey,range.stopKeyInclusive);

	}

	static org::apache::accumulo::core::data::thrift::ScanBatch convert(
	        std::vector<interconnect::ScanIdentifier<cclient::data::KeyExtent*, cclient::data::Range*> *> *rangeIdentifiers)
	{
	   
		org::apache::accumulo::core::data::thrift::ScanBatch littleBatch;

		//typedef std::map<class TKeyExtent, std::std::vector<class Tcclient::data::Range> >  ScanBatch;
		for (auto it : *rangeIdentifiers) {

			interconnect::ScanIdentifier<cclient::data::KeyExtent*, cclient::data::Range*> *identifier = it;
			std::vector<cclient::data::KeyExtent*> extents = identifier->getGlobalMapping();
			for (cclient::data::KeyExtent *ot : extents) {
				org::apache::accumulo::core::data::thrift::TKeyExtent keyExtent;

				keyExtent.table = ot->getTableId();
				std::string endRow = ot->getEndRow();
				if (!IsEmpty(&endRow))
					keyExtent.endRow = ot->getEndRow();

				std::string prevEndRow = ot->getEndRow();
				if (!IsEmpty(&prevEndRow))
					keyExtent.prevEndRow = ot->getPrevEndRow();
				
				std::vector<org::apache::accumulo::core::data::thrift::TRange> thriftRanges;
				std::vector<cclient::data::Range*> ranges = identifier->getIdentifiers(ot);
				for (cclient::data::Range *rt : ranges) {

					org::apache::accumulo::core::data::thrift::TRange range = convert(rt);
					thriftRanges.push_back(range);

				}
				littleBatch.insert(
				        std::pair<org::apache::accumulo::core::data::thrift::TKeyExtent, std::vector<org::apache::accumulo::core::data::thrift::TRange>>(keyExtent,
				                        thriftRanges));
			}

		}

		return littleBatch;
	}

	static org::apache::accumulo::core::data::thrift::TKey convert(cclient::data::Key *key)
	{
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

	static cclient::data::Key *convert(org::apache::accumulo::core::data::thrift::TKey key)
	{
		cclient::data::Key *newKey = new cclient::data::Key();
		newKey->setRow(key.row);
		newKey->setColFamily(key.colFamily);
		newKey->setColQualifier(key.colQualifier);
		newKey->setColVisibility(key.colVisibility);
		newKey->setTimeStamp(key.timestamp);
		return newKey;
	}

	static org::apache::accumulo::core::data::thrift::TKeyExtent convert(
	        cclient::data::KeyExtent *ot)
	{
		org::apache::accumulo::core::data::thrift::TKeyExtent keyExtent;
		keyExtent.table = ot->getTableId();
		std::string endRow = ot->getEndRow();

		if (!IsEmpty(&endRow))
			keyExtent.endRow = endRow;
		else
		  keyExtent.endRow="";

		std::string prevEndRow = ot->getPrevEndRow();

		if (!IsEmpty(&prevEndRow))
			keyExtent.prevEndRow = prevEndRow;
		else
		  keyExtent.prevEndRow="";
		
		
		return keyExtent;
	}

	static std::vector<cclient::data::KeyValue*> *convert(
	        std::vector<org::apache::accumulo::core::data::thrift::TKeyValue> tkvVec)
	{
		std::vector<cclient::data::KeyValue*> *newvector = new std::vector<cclient::data::KeyValue*>();
		cclient::data::Key *prevKey = NULL;

		for (uint32_t i = 0; i < tkvVec.size(); i++) {

			org::apache::accumulo::core::data::thrift::TKeyValue tkv =
			        tkvVec.at(i);
			cclient::data::Key *key = new cclient::data::Key();
			if (!IsEmpty(&(tkv.key.row))) {
				key->setRow(tkv.key.row.c_str(), tkv.key.row.size());
			} else {
				if (NULL != prevKey) {
					std::pair<char*, size_t> prevRow = prevKey->getRow();
					key->setRow(prevRow.first, prevRow.second);
				}

			}
			if (!IsEmpty(&(tkv.key.colFamily))) {
				key->setColFamily(tkv.key.colFamily.c_str(),
				                  tkv.key.colFamily.size());
			} else {
				if (NULL != prevKey) {
					std::pair<char*, size_t> prevCf = prevKey->getColFamily();
					key->setColFamily(prevCf.first, prevCf.second);
				}
			}
			if (!IsEmpty(&(tkv.key.colQualifier))) {
				key->setColQualifier(tkv.key.colQualifier.c_str(),
				                     tkv.key.colQualifier.size());
			} else {
				if (NULL != prevKey) {
					std::pair<char*, size_t> prevCq =
					        prevKey->getColQualifier();
					key->setColQualifier(prevCq.first, prevCq.second);
				}
			}

			if (!IsEmpty(&(tkv.key.colVisibility))) {
				key->setColVisibility(tkv.key.colVisibility.c_str(),
				                      tkv.key.colVisibility.size());
			} else {
				if (NULL != prevKey) {
					std::pair<char*, size_t> prevCv =
					        prevKey->getColVisibility();
					key->setColVisibility(prevCv.first, prevCv.second);
				}
			}

			key->setTimeStamp(tkv.key.timestamp);
			cclient::data::KeyValue *kv = new cclient::data::KeyValue();
			kv->setKey(key,true);
			kv->setValue((uint8_t*) tkv.value.c_str(), tkv.value.size());

			newvector->push_back(kv);

			prevKey = key;
		}
		return newvector;
	}

	static org::apache::accumulo::core::data::thrift::TRange convert(
	        cclient::data::Range *range)
	{
		org::apache::accumulo::core::data::thrift::TRange newRange;
		if (!range->getInfiniteStartKey() && NULL  != range->getStartKey())
			newRange.start = convert(range->getStartKey());

		if (!range->getInfiniteStopKey() && NULL != range->getStopKey())
			newRange.stop = convert(range->getStopKey());

		newRange.startKeyInclusive = range->getStartKeyInclusive();
		newRange.stopKeyInclusive = range->getStopKeyInclusive();
		newRange.infiniteStartKey = range->getInfiniteStartKey();
		newRange.infiniteStopKey = range->getInfiniteStopKey();
		return newRange;
	}

	static std::vector<org::apache::accumulo::core::data::thrift::TColumn> convert(
	        std::vector<cclient::data::Column*> *columns)
	{

		std::vector<org::apache::accumulo::core::data::thrift::TColumn> convertedColumns;

		if (!IsEmpty(columns)) {
			for (auto it = columns->begin(); it != columns->end(); it++) {
				org::apache::accumulo::core::data::thrift::TColumn column;
				std::pair<char*, size_t> cf = (*it)->getColFamily();
				column.columnFamily = std::string(cf.first, cf.second);
				std::pair<char*, size_t> cq = (*it)->getColQualifier();
				if (cq.second > 0)
					column.columnQualifier = std::string(cq.first, cq.second);
				std::pair<char*, size_t> cv = (*it)->getColVisibility();
				if (cv.second > 0)
					column.columnVisibility = std::string(cv.first, cv.second);

				convertedColumns.push_back(column);
			}
		}

		return convertedColumns;

	}

	static std::vector<std::string> convert(cclient::data::security::Authorizations *auths)
	{

		return auths->getAuthorizations();

	}

protected:
	ThriftWrapper()
	{

	}
	virtual ~ThriftWrapper()
	{

	}
}
;

} /* namespace interconnect */
#endif /* THRIFTWRAPPER_H_ */

