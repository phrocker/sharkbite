CPPFLAGS=-g --std=c++11 -lthrift -I/usr/local/include/thrift/ -fPIC
LDFLAGS=-g
LDLIBS=
RM=rm -f


OBJS=$(subst .cpp,.o,$(wildcard *.cpp))


all: scanrequest

clean: 
	$(RM) $(OBJS)


scanrequest:	$(OBJS)
	g++ -o scanrequest.so $(OBJS) $(LDFLAGS) -shared -fPIC
	$(RM) $(OBJS)
