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
 * See the License for the specific s governing permissions and
 * limitations under the License.
 */

#ifndef RESULTS_H_
#define RESULTS_H_


#include <set>
#include <string>
#include <boost/lockfree/queue.hpp>



#include "SourceConditions.h"
#include "../../data/constructs/inputvalidation.h"

namespace scanners {
template<typename T> class ResultBlock;
template<typename T> class ResultIter;
template<typename T, class BlockType> class Results;

/**

 **/
template<typename T>
class ResultBlock: public std::iterator<std::forward_iterator_tag, T> {
protected:

    SourceConditions *sourceConditionals;

    boost::lockfree::queue<T*> *resultSet;
    
   

    T *current;

    bool isEnd;

    virtual void setEnd(bool val) {
        isEnd = val;
    }

public:

    ResultBlock(SourceConditions *conditionals,
                boost::lockfree::queue<T*> *queue, bool setEnd = false) :
        isEnd(setEnd) {
        resultSet = queue;

        sourceConditionals = conditionals;
    }

    ResultBlock<T> begin() {
        getNextResult();
        return *this;
    }

    bool isEndOfRange() const {
        return isEnd;
    }

    ResultBlock<T> end() {
        return static_cast<ResultBlock<T> >(ResultBlock(sourceConditionals,
                                            resultSet, true));
    }
    
    SourceConditions *getSourceConditionals() const
    {
      return sourceConditionals;
    }
    
    boost::lockfree::queue<T*> *getResultSet() const
    {
      return resultSet;
    }
    

    std::unique_ptr<T> operator*() {

        return std::move(std::unique_ptr<T>(current));
    }

    inline void getNextResult() {
        do {
	    
            if (!resultSet->pop(current)) {
                sourceConditionals->waitForResults();
                if (resultSet->pop(current))
		{
		    sourceConditionals->decrementCount();
                    break;
		}
                if (!sourceConditionals->isAlive()
                        && sourceConditionals->size() <= 0) {
                    isEnd = true;
                }

            } else
	    {
	      sourceConditionals->decrementCount();
                break;
	    }
        } while (sourceConditionals->isAlive());

    }
    ResultBlock& operator++() {
        getNextResult();
        return *this;
    }

    ResultBlock& operator++(int t) {
        for (int i = 0; i < (t + 1); i++) {
            getNextResult();
        }

        return *this;
    }

    ResultBlock& operator=(const ResultBlock &rhs) {
        resultSet = rhs.resultSet;
        sourceConditionals = rhs.sourceConditionals;
        isEnd = rhs.isEnd;
        return *this;
    }

    bool operator==(const ResultIter<std::unique_ptr<T>> &rhs) {
        return isEnd == rhs.getParent();
    }

    bool operator!=(const ResultBlock &rhs) {
        return !(isEnd == rhs.isEnd);
    }

    void add(T *t) {
        resultSet->push(t);
	
        sourceConditionals->awakeThreadsForResults();
    }

    void add(std::vector<std::unique_ptr<T>> *t) {
        for (typename std::vector<std::unique_ptr<T>>::iterator it = t->begin(); it != t->end();
                it++) {
            resultSet->push(*it.release());
	    sourceConditionals->incrementCount();
        }

        sourceConditionals->awakeThreadsForResults();
    }
    
    void add(std::vector<T*> *t) {
        for (typename std::vector<T*>::iterator it = t->begin(); it != t->end();
                it++) {
            resultSet->push(*it);
	    sourceConditionals->incrementCount();
        }

        sourceConditionals->awakeThreadsForResults();
    }

    virtual ~ResultBlock() {

    }
    friend class ResultIter<T*> ;
};

template<typename T>
class ResultIter: public ResultBlock<T> {
protected:
    ResultBlock<T> *parent;

    void setEnd(bool val) {
        ResultBlock<T>::setEnd(val);
    }
public:

    ResultIter(ResultBlock<T> *copyResultSet, bool end = false) :
        ResultBlock<T>(copyResultSet->getSourceConditionals(),
                       copyResultSet->getResultSet(),
                       (end ? end : copyResultSet->isEndOfRange()))

    {
        setEnd((end ? end : copyResultSet->isEndOfRange()));
        parent = copyResultSet;
    }

    ResultIter(ResultIter<T> *copyResultSet, bool end = false) :
        ResultBlock<T>(copyResultSet->getSourceConditionals(),
                       copyResultSet->getResultSet(),
                       (end ? end : copyResultSet->isEndOfRange()))

    {
        setEnd((end ? end : copyResultSet->isEndOfRange()));
        parent = copyResultSet;
    }

    ResultBlock<T> begin() {
        return *this;
    }

    ResultBlock<T> end() {

        return parent->end();
    }

    ResultBlock<T> *getParent() {
        return parent;
    }

    std::unique_ptr<T> operator*() {
        return std::move((parent->operator*()));
    }

    ResultIter& operator++() {
        (*parent)++;
        return *this;
    }

    ResultIter& operator++(int t) {
        (*parent)++;
        return *this;
    }

    bool isEndOfRange() const{
        return parent->isEndOfRange();
    }

    bool operator==(const ResultIter &rhs) {
        return *(*parent) == *(*rhs.parent);
    }

    bool operator==(const ResultBlock<T> &rhs) {
        return parent->isEndOfRange() == rhs.isEndOfRange();
    }

    bool operator!=(const ResultIter<T> &rhs) {
        return !(*this == rhs);
    }

    bool operator!=(const ResultBlock<T> &rhs) {
        return !(*this == rhs);
    }

    virtual ~ResultIter() {

    }
};

template<typename T, class BlockType>
class Results {
protected:

    boost::lockfree::queue<T*> resultSet;

    BlockType *iter;

    SourceConditions conditions;

    volatile uint16_t producers;

public:

    typedef BlockType iterator;

    Results() : resultSet(2000) /*-> decltype(static_cast<BlockType>(T))*/
    {
        iter = new BlockType(&conditions, &resultSet);

        producers = 0;
    }

    void add(T *t) {
        iter->add(t);
    }

/*    void add(std::vector<std::unique_ptr<T>> *t) {
        iter->add(t);
    }
    */
    
    void add_ptr(std::vector<T*> *t) {
        iter->add(t);
    }

    iterator begin() {
        return iter->begin();
    }

    iterator end() {
        return iter->end();
    }

    void decrementProducers() {
        if (--producers <= 0) {

            conditions.awakeThreadsFinished();
        }
    }

    void registerProducer() {
        producers++;
    }

    ~Results() {
    }

protected:
 

};
} /* namespace scanners */
#endif /* RESULTS_H_ */
