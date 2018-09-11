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
#ifndef SRC_CLIENTEXAMPLE_H_
#define SRC_CLIENTEXAMPLE_H_



#ifdef __cplusplus
extern "C" {
#endif
#include "data/constructs/Key.h"
#include "data/constructs/Mutation.h"
#include "data/constructs/KeyValue.h"
#include "data/constructs/security/AuthInfo.h"
#include "data/constructs/security/Authorizations.h"
#include "scanner/constructs/Results.h"
#include "scanner/impl/Scanner.h"
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include "accumulo_data.h"
#include <data/constructs/Key.h>

class ScanRes{
	scanners::ResultBlock<cclient::data::KeyValue> st;

	std::shared_ptr<cclient::data::KeyValue> kv_;

	std::shared_ptr<cclient::data::Key> key_;

	scanners::Iterator<cclient::data::KeyValue> *results;
	scanners::BatchScanner *scan_;

public:
	ScanRes(scanners::BatchScanner *scan) : scan_(scan) {
		results = scan_->getResultSet();
		st =results->begin();
	}

	bool hasNext(){
	    bool hasNext =  st != results->end();
	    if (hasNext){
	        st.getNextResult();
            kv_ = *st;
            if (kv_ == nullptr){
                std::cout  << "kv is null" << std::endl;
                return false;
            }
            key_ = kv_->getKey();

	    }
		return hasNext;

	}


	std::shared_ptr<cclient::data::Key> next(){
        st++;
		return key_;
	}


};
struct TableOps {
	char *table_name;
	// stuff you don't use
	struct connector *parent;
	void *tableOpsPtr;
};

struct BatchScan {
	struct TableOps *tableOps;
	// stuff you don't use
	void *scannerPtr;
	void *res;
	void *on_next_fn;
};

struct BatchWriter {
	// stuff you don't use
	void *writerPtr;
};

struct CMutation {
    void *mutationPtr;
};

struct connector {

	// stuff you don't use
	void *masterPtr;
	void *zk;

};



struct connector *create_connector(char *instance, char *zks, char *username,
		char *password);

int free_connector(struct connector *connector);

struct TableOps *open_table(struct connector *connector, char *tableName);

struct TableOps *create_table(struct connector *connector, char *tableName);

int drop_table(struct TableOps *tableOps);

int free_table(struct TableOps *tableOps);

// scanner code

struct CMutation *createMutation(char *row);

void freeMutation(struct CMutation *);

void put(struct CMutation *, char *cf, char *cq, char *cv);

struct BatchScan *createScanner(struct TableOps *tableOps, short threads);

struct BatchWriter *createWriter(struct TableOps *tableOps, short threads);

int addMutation(struct BatchWriter *writer, struct CMutation *mutation);

int addRange(struct BatchScan *scanner, struct CRange *range);

int addRanges(struct BatchScan *scanner, struct CRange **range, int size);

bool hasNext(struct BatchScan *scanner);

int onNext(struct BatchScan *scanner, struct CKeyValue *kv);

CKeyValue *next(struct BatchScan *scanner);

int nextMany(struct BatchScan *scanner, struct KeyValueList *kvl);

int closeScanner(struct BatchScan *scanner);

int closeWriter(struct BatchWriter *writer);



struct CKey *createKey(char *row, char *cf, char *cq, char *cv, uint64_t timestamp);

// writer code

std::shared_ptr<cclient::data::Key> toKey(CKey *key);

void populateKey(CKey *key, const std::shared_ptr<cclient::data::Key> &otherKey);

#ifdef __cplusplus
}

//end extern "C"
#endif

#endif
