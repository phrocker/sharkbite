CPPFLAGS=-O3 --std=c++11 -lthrift -I/usr/local/include/thrift/
LDFLAGS=
LDLIBS=
RM=rm -f

SRCS=IndexEntry.cpp LocalityGroupMetaData.cpp MetaBlock.cpp
OBJS=$(subst .cpp,.o,$(SRCS))


all: meta

clean: 
	$(RM) $(OBJS)


meta:	$(OBJS)
	g++ -o meta.so $(OBJS) -shared -fPIC
	$(RM) $(OBJS)

IndexEntry.o: IndexEntry.cpp
	g++ $(CPPFLAGS) $(LDFLAGS) -c IndexEntry.cpp -fPIC

LocalityGroupMetaData.o: LocalityGroupMetaData.cpp
	g++ $(CPPFLAGS) $(LDFLAGS) -c LocalityGroupMetaData.cpp -fPIC
	
MetaBlock.o: MetaBlock.cpp
	g++ $(CPPFLAGS) $(LDFLAGS) -c MetaBlock.cpp -fPIC
	